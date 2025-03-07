package apidump

import (
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pkg/errors"
	"github.com/spf13/viper"

	"github.com/akitasoftware/akita-cli/ci"
	"github.com/akitasoftware/akita-cli/deployment"
	"github.com/akitasoftware/akita-cli/location"
	"github.com/akitasoftware/akita-cli/pcap"
	"github.com/akitasoftware/akita-cli/plugin"
	"github.com/akitasoftware/akita-cli/printer"
	"github.com/akitasoftware/akita-cli/rest"
	"github.com/akitasoftware/akita-cli/tcp_conn_tracker"
	"github.com/akitasoftware/akita-cli/tls_conn_tracker"
	"github.com/akitasoftware/akita-cli/trace"
	"github.com/akitasoftware/akita-cli/util"
	"github.com/akitasoftware/akita-libs/akid"
	"github.com/akitasoftware/akita-libs/akiuri"
	"github.com/akitasoftware/akita-libs/tags"
)

// TODO(kku): make pcap timings more robust (e.g. inject a sentinel packet to
// mark start and end of pcap).
const (
	// Empirically, it takes 1s for pcap to be ready to process packets.
	// We budget for 5x to be safe.
	pcapStartWaitTime = 5 * time.Second

	// Empirically, it takes 1s for the first packet to become available for
	// processing.
	// We budget for 5x to be safe.
	pcapStopWaitTime = 5 * time.Second
)

const (
	subcommandOutputDelimiter = "======= _AKITA_SUBCOMMAND_ ======="
)

type filterState string

const (
	matchedFilter    filterState = "MATCHED"
	notMatchedFilter filterState = "UNMATCHED"
)

type Args struct {
	// Required args
	ClientID akid.ClientID
	Domain   string

	// Optional args

	// If both LocalPath and AkitaURI are set, data is teed to both local traces
	// and backend trace.
	// If unset, defaults to a random spec name on Akita Cloud.
	Out location.Location

	Interfaces     []string
	Filter         string
	Tags           map[tags.Key]string
	PathExclusions []string
	HostExclusions []string
	PathAllowlist  []string
	HostAllowlist  []string

	// Rate-limiting parameters -- only one should be set to a non-default value.
	SampleRate         float64
	WitnessesPerMinute float64

	// If set, apidump will run the command in a subshell and terminate
	// automatically when the subcommand terminates.
	//
	// apidump will pipe stdout and stderr from the command. If the command stops
	// with non-zero exit code, apidump will also exit with the same exit code.
	ExecCommand string

	// Username to run ExecCommand as. If not set, defaults to the current user.
	ExecCommandUser string

	Plugins []plugin.AkitaPlugin
}

func (args *Args) lint() {
	// Modifies the input to remove empty strings. Returns true if the input was
	// modified.
	removeEmptyStrings := func(strings []string) ([]string, bool) {
		i := 0
		modified := false
		for _, elt := range strings {
			if len(elt) > 0 {
				strings[i] = elt
				i++
			} else {
				modified = true
			}
		}
		strings = strings[:i]
		return strings, modified
	}

	// Empty path/host-exclusion regular expressions will exclude everything.
	// Ignore these and print a warning.
	for paramName, argsPtr := range map[string]*[]string{
		"--path-exclusions": &args.PathExclusions,
		"--host-exclusions": &args.HostExclusions,
	} {
		modified := false
		*argsPtr, modified = removeEmptyStrings(*argsPtr)
		if modified {
			printer.Stderr.Warningf("Ignoring empty regex in %s, which would otherwise exclude everything\n", paramName)
		}
	}

	// Empty path/host-inclusion regular expressions will include everything. If
	// there are any non-empty regular expressions, ignore the empty regexes and
	// print a warning.
	for paramName, argsPtr := range map[string]*[]string{
		"--path-allow": &args.PathAllowlist,
		"--host-allow": &args.HostAllowlist,
	} {
		modified := false
		*argsPtr, modified = removeEmptyStrings(*argsPtr)
		if modified && len(*argsPtr) > 0 {
			printer.Stderr.Warningf("Ignoring empty regex in %s, which would otherwise include everything\n", paramName)
		}
	}
}

// DumpPacketCounters prints the accumulated packet counts per interface and per port,
// at Debug level, to stderr.  The first argument should be the keyed by interface names (as created
// in the Run function below); all we really need are those names.
func DumpPacketCounters(interfaces map[string]interfaceInfo, matchedSummary *trace.PacketCountSummary, unmatchedSummary *trace.PacketCountSummary, showInterface bool) {
	// Using a map gives inconsistent order when iterating (even on the same run!)
	filterStates := []filterState{matchedFilter, notMatchedFilter}
	toReport := []*trace.PacketCountSummary{matchedSummary}
	if unmatchedSummary != nil {
		toReport = append(toReport, unmatchedSummary)
	}

	if showInterface {
		printer.Stderr.Debugf("==================================================\n")
		printer.Stderr.Debugf("Packets per interface:\n")
		printer.Stderr.Debugf("%15v %8v %7v %11v %5v\n", "", "", "TCP  ", "HTTP   ", "")
		printer.Stderr.Debugf("%15v %8v %7v %5v %5v %5v\n", "interface", "dir", "packets", "req", "resp", "unk")
		for n := range interfaces {
			for i, summary := range toReport {
				count := summary.TotalOnInterface(n)
				printer.Stderr.Debugf("%15s %9s %7d %5d %5d %5d\n",
					n,
					filterStates[i],
					count.TCPPackets,
					count.HTTPRequests,
					count.HTTPResponses,
					count.Unparsed,
				)
			}
		}
	}

	printer.Stderr.Debugf("==================================================\n")
	printer.Stderr.Debugf("Packets per port:\n")
	printer.Stderr.Debugf("%8v %7v %11v %5v\n", "", "TCP  ", "HTTP   ", "")
	printer.Stderr.Debugf("%8v %7v %5v %5v %5v\n", "port", "packets", "req", "resp", "unk")
	for i, summary := range toReport {
		if filterStates[i] == matchedFilter {
			printer.Stderr.Debugf("--------- matching filter --------\n")
		} else {
			printer.Stderr.Debugf("------- not matching filter ------\n")
		}
		byPort := summary.AllPorts()
		// We don't really know what's in the BPF filter; we know every packet in
		// matchedSummary must have matched, but that could be multiple ports, or
		// some other criteria.
		for _, count := range byPort {
			printer.Stderr.Debugf("%8d %7d %5d %5d %5d\n",
				count.SrcPort,
				count.TCPPackets,
				count.HTTPRequests,
				count.HTTPResponses,
				count.Unparsed,
			)
		}
		if len(byPort) == 0 {
			printer.Stderr.Debugf("       no packets captured        \n")
		}
	}

	printer.Stderr.Debugf("==================================================\n")

}

// args.Tags may be initialized via the command line, but automated settings
// are mainly performed here (for now.)
func collectTraceTags(args *Args) map[tags.Key]string {
	traceTags := args.Tags
	if traceTags == nil {
		traceTags = map[tags.Key]string{}
	}
	// Store the current packet capture flags so we can reuse them in active
	// learning.
	if len(args.Interfaces) > 0 {
		traceTags[tags.XAkitaDumpInterfacesFlag] = strings.Join(args.Interfaces, ",")
	}
	if args.Filter != "" {
		traceTags[tags.XAkitaDumpFilterFlag] = args.Filter
	}

	// Set CI type and tags on trace
	ciType, _, ciTags := ci.GetCIInfo()
	if ciType != ci.Unknown {
		for k, v := range ciTags {
			traceTags[k] = v
		}
		traceTags[tags.XAkitaSource] = tags.CISource
	}

	// Import information about production or staging environment
	deployment.UpdateTags(traceTags)

	// Set source to user by default (if not CI or deployment)
	if _, ok := traceTags[tags.XAkitaSource]; !ok {
		traceTags[tags.XAkitaSource] = tags.UserSource
	}

	printer.Debugln("trace tags:", traceTags)
	return traceTags
}

func compileRegexps(filters []string, name string) ([]*regexp.Regexp, error) {
	result := make([]*regexp.Regexp, len(filters))
	for i, f := range filters {
		r, err := regexp.Compile(f)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to compile %s %q", name, f)
		}
		result[i] = r
	}
	return result, nil
}

// Captures packets from the network and adds them to a trace. The trace is
// created if it doesn't already exist.
func Run(args Args) error {
	args.lint()

	// During debugging, capture packets not matching the user's filters so we can
	// report statistics on those packets.
	capturingNegation := viper.GetBool("debug")

	if capturingNegation {
		printer.Debugln("Capturing filtered traffic for debugging.")
	}

	// Get the interfaces to listen on.
	interfaces, err := getEligibleInterfaces(args.Interfaces)
	if err != nil {
		return errors.Wrap(err, "failed to list network interfaces")
	}

	// Build the user-specified filter and its negation for each interface.
	userFilters, negationFilters, err := createBPFFilters(interfaces, args.Filter, capturingNegation, 0)
	if err != nil {
		return err
	}
	printer.Debugln("User-specified BPF filters:", userFilters)
	if capturingNegation {
		printer.Debugln("Negation BPF filters:", negationFilters)
	}

	traceTags := collectTraceTags(&args)

	// Build path filters.
	pathExclusions, err := compileRegexps(args.PathExclusions, "path exclusion")
	if err != nil {
		return err
	}
	hostExclusions, err := compileRegexps(args.HostExclusions, "host exclusion")
	if err != nil {
		return err
	}
	pathAllowlist, err := compileRegexps(args.PathAllowlist, "path filter")
	if err != nil {
		return err
	}
	hostAllowlist, err := compileRegexps(args.HostAllowlist, "host filter")
	if err != nil {
		return err
	}

	// Validate args.Out and fill in any missing defaults.
	if uri := args.Out.AkitaURI; uri != nil {
		if uri.ObjectType == nil {
			uri.ObjectType = akiuri.TRACE.Ptr()
		} else if !uri.ObjectType.IsTrace() {
			return errors.Errorf("%q is not an Akita trace URI", uri)
		}

		// Use a random object name by default.
		if uri.ObjectName == "" {
			uri.ObjectName = util.RandomLearnSessionName()
		}
	}

	// If the output is targeted at the backend, create a shared backend
	// learn session.
	var backendSvc akid.ServiceID
	var backendLrn akid.LearnSessionID
	var learnClient rest.LearnClient
	if uri := args.Out.AkitaURI; uri != nil {
		frontClient := rest.NewFrontClient(args.Domain, args.ClientID)
		backendSvc, err = util.GetServiceIDByName(frontClient, uri.ServiceName)
		if err != nil {
			return err
		}
		learnClient = rest.NewLearnClient(args.Domain, args.ClientID, backendSvc)

		backendLrn, err = util.NewLearnSession(args.Domain, args.ClientID, backendSvc, uri.ObjectName, traceTags, nil)
		if err == nil {
			printer.Infof("Created new trace on Akita Cloud: %s\n", uri)
		} else {
			var httpErr rest.HTTPError
			if ok := errors.As(err, &httpErr); ok && httpErr.StatusCode == 409 {
				backendLrn, err = util.GetLearnSessionIDByName(learnClient, uri.ObjectName)
				if err != nil {
					return errors.Wrapf(err, "failed to lookup ID for existing trace %s", uri)
				}
				printer.Infof("Adding to existing trace: %s\n", uri)
			} else {
				return errors.Wrap(err, "failed to create or fetch trace already")
			}
		}
	}

	// Initialize packet counts
	filterSummary := trace.NewPacketCountSummary()
	negationSummary := trace.NewPacketCountSummary()

	numUserFilters := len(pathExclusions) + len(hostExclusions) + len(pathAllowlist) + len(hostAllowlist)
	prefilterSummary := trace.NewPacketCountSummary()

	// Initialized shared rate object, if we are configured with a rate limit
	var rateLimit *trace.SharedRateLimit
	if args.WitnessesPerMinute != 0.0 {
		rateLimit = trace.NewRateLimit(args.WitnessesPerMinute)
		defer rateLimit.Stop()
	}

	// Start collecting
	var doneWG sync.WaitGroup
	doneWG.Add(len(userFilters) + len(negationFilters))
	errChan := make(chan error, len(userFilters)+len(negationFilters)) // buffered enough so it never blocks
	stop := make(chan struct{})
	for _, filterState := range []filterState{matchedFilter, notMatchedFilter} {
		var summary *trace.PacketCountSummary
		var filters map[string]string
		if filterState == matchedFilter {
			filters = userFilters
			summary = filterSummary
		} else {
			filters = negationFilters
			summary = negationSummary
		}

		for interfaceName, filter := range filters {
			var collector trace.Collector

			// Build collectors from the inside out (last applied to first applied).
			//  8. Back-end collector (sink).
			//  7. Statistics.
			//  6. Subsampling.
			//  5. Path and host filters.
			//  4. Eliminate Akita CLI traffic.
			//  3. Count packets before user filters for diagnostics.
			//  2. Process TLS traffic into TLS-connection metadata.
			//  1. Aggregate TCP-packet metadata into TCP-connection metadata.

			// Back-end collector (sink).
			if filterState == notMatchedFilter {
				// During debugging, we capture the negation of the user's filters. This
				// allows us to report statistics for packets not matching the user's
				// filters. We need to avoid sending this traffic to the back end,
				// however.
				collector = trace.NewDummyCollector()
			} else {
				var localCollector trace.Collector
				if args.Out.LocalPath != nil {
					if lc, err := createLocalCollector(interfaceName, *args.Out.LocalPath, traceTags); err == nil {
						localCollector = lc
					} else {
						return err
					}
				}

				if args.Out.AkitaURI != nil && args.Out.LocalPath != nil {
					collector = trace.TeeCollector{
						Dst1: trace.NewBackendCollector(backendSvc, backendLrn, learnClient, args.Plugins),
						Dst2: localCollector,
					}
				} else if args.Out.AkitaURI != nil {
					collector = trace.NewBackendCollector(backendSvc, backendLrn, learnClient, args.Plugins)
				} else if args.Out.LocalPath != nil {
					collector = localCollector
				} else {
					return errors.Errorf("invalid output location")
				}
			}

			// Statistics.
			//
			// Count packets that have *passed* filtering (so that we know whether the
			// trace is empty or not.)  In the future we could add columns for both
			// pre- and post-filtering.
			collector = &trace.PacketCountCollector{
				PacketCounts: summary,
				Collector:    collector,
			}

			// Subsampling.
			collector = trace.NewSamplingCollector(args.SampleRate, collector)
			if rateLimit != nil {
				collector = rateLimit.NewCollector(collector)
			}

			// Path and host filters.
			if len(hostExclusions) > 0 {
				collector = trace.NewHTTPHostFilterCollector(hostExclusions, collector)
			}
			if len(pathExclusions) > 0 {
				collector = trace.NewHTTPPathFilterCollector(pathExclusions, collector)
			}
			if len(hostAllowlist) > 0 {
				collector = trace.NewHTTPHostAllowlistCollector(hostAllowlist, collector)
			}
			if len(pathAllowlist) > 0 {
				collector = trace.NewHTTPPathAllowlistCollector(pathAllowlist, collector)
			}

			// Eliminate Akita CLI traffic, unless --dogfood has been specified
			if !viper.GetBool("dogfood") {
				collector = &trace.UserTrafficCollector{
					Collector: collector,
				}
			}

			// Count packets before user filters for diagnostics
			if filterState == matchedFilter && numUserFilters > 0 {
				collector = &trace.PacketCountCollector{
					PacketCounts: prefilterSummary,
					Collector:    collector,
				}
			}

			// Process TLS traffic into TLS-connection metadata.
			collector = tls_conn_tracker.NewCollector(collector)

			// Process TCP-packet metadata into TCP-connection metadata.
			collector = tcp_conn_tracker.NewCollector(collector)

			// Compute the share of the page cache that each collection process may use.
			// (gopacket does not currently permit a unified page cache for packet reassembly.)
			bufferShare := 1.0 / float32(len(negationFilters)+len(userFilters))

			go func(interfaceName, filter string) {
				defer doneWG.Done()
				// Collect trace. This blocks until stop is closed or an error occurs.
				if err := trace.Collect(stop, interfaceName, filter, bufferShare, collector, summary); err != nil {
					errChan <- errors.Wrapf(err, "failed to collect trace on interface %s", interfaceName)
				}
			}(interfaceName, filter)
		}
	}

	{
		iNames := make([]string, 0, len(interfaces))
		for n := range interfaces {
			iNames = append(iNames, n)
		}
		printer.Stderr.Infof("Running learn mode on interfaces %s\n", strings.Join(iNames, ", "))
	}

	unfiltered := true
	for _, f := range userFilters {
		if f != "" {
			unfiltered = false
			break
		}
	}
	if unfiltered {
		printer.Stderr.Warningf("%s\n", printer.Color.Yellow("--filter flag is not set, this means that all network traffic is treated as your API traffic"))
	}

	var stopErr error
	if args.ExecCommand != "" {
		printer.Stderr.Infof("Running subcommand...\n\n\n")

		time.Sleep(pcapStartWaitTime)

		// Print delimiter so it's easier to differentiate subcommand output from
		// Akita output.
		// It won't appear in JSON-formatted output.
		printer.Stdout.RawOutput(subcommandOutputDelimiter)
		printer.Stderr.RawOutput(subcommandOutputDelimiter)
		cmdErr := runCommand(args.ExecCommandUser, args.ExecCommand)
		printer.Stdout.RawOutput(subcommandOutputDelimiter)
		printer.Stderr.RawOutput(subcommandOutputDelimiter)

		if cmdErr != nil {
			stopErr = errors.Wrap(cmdErr, "failed to run subcommand")
			// We promised to preserve the subcommand's exit code.
			// Explicitly notify whoever is running us to exit.
			if exitErr, ok := errors.Cause(stopErr).(*exec.ExitError); ok {
				stopErr = util.ExitError{
					ExitCode: exitErr.ExitCode(),
					Err:      stopErr,
				}
			}
		} else {
			// Check if we have any errors on our side.
			select {
			case err := <-errChan:
				stopErr = err
				printer.Stderr.Errorf("Encountered error while collecting traces, stopping...\n")
			default:
				printer.Stderr.Infof("Subcommand finished successfully, stopping trace collection...\n")
			}
		}
	} else {
		// Don't sleep pcapStartWaitTime in interactive mode since the user can send
		// SIGINT while we're sleeping too and sleeping introduces visible lag.
		printer.Stderr.Infof("Send SIGINT (Ctrl-C) to stop...\n")

		// Set up signal handler to stop packet processors on SIGINT or when one of
		// the processors returns an error.
		{
			// Must use buffered channel for signals since the signal package does not
			// block when sending signals.
			sig := make(chan os.Signal, 2)
			signal.Notify(sig, os.Interrupt)
			signal.Notify(sig, syscall.SIGTERM)
			select {
			case received := <-sig:
				printer.Stderr.Infof("Received %v, stopping trace collection...\n", received.String())
			case err := <-errChan:
				stopErr = err
				printer.Stderr.Errorf("Encountered error while collecting traces, stopping...\n")
			}
		}
	}

	time.Sleep(pcapStopWaitTime)

	// Signal all processors to stop.
	close(stop)

	// Wait for processors to exit.
	doneWG.Wait()
	if stopErr != nil {
		return errors.Wrap(stopErr, "trace collection failed")
	}

	if viper.GetBool("debug") {
		if len(negationFilters) == 0 {
			DumpPacketCounters(interfaces, filterSummary, nil, true)
		} else {
			DumpPacketCounters(interfaces, filterSummary, negationSummary, true)
		}

		if numUserFilters > 0 {
			printer.Stderr.Debugf("+++ Counts before allow and exclude filters and sampling +++\n")
			DumpPacketCounters(interfaces, prefilterSummary, nil, false)
		}

	}

	// Report on recoverable error counts during trace
	if pcap.CountNilAssemblerContext > 0 || pcap.CountNilAssemblerContextAfterParse > 0 || pcap.CountBadAssemblerContextType > 0 {
		printer.Stderr.Infof("Detected packet assembly context problems during capture: %v empty, %v bad type, %v empty after parse",
			pcap.CountNilAssemblerContext,
			pcap.CountBadAssemblerContextType,
			pcap.CountNilAssemblerContextAfterParse)
		printer.Stderr.Infof("These errors may cause some packets to be missing from the trace.")
	}

	// Check summary to see if the trace will have anything in it.
	totalCount := filterSummary.Total()
	if totalCount.HTTPRequests == 0 && totalCount.HTTPResponses == 0 {
		// TODO: recognize TLS handshakes and count them separately!
		if totalCount.TCPPackets == 0 {
			if capturingNegation && negationSummary.Total().TCPPackets == 0 {
				printer.Stderr.Infof("Did not capture any TCP packets during the trace.\n")
				printer.Stderr.Infof("%s\n", printer.Color.Yellow("This may mean the traffic is on a different interface, or that"))
				printer.Stderr.Infof("%s\n", printer.Color.Yellow("there is a problem sending traffic to the API."))
			} else {
				printer.Stderr.Infof("Did not capture any TCP packets matching the filter.\n")
				printer.Stderr.Infof("%s\n", printer.Color.Yellow("This may mean your filter is incorrect, such as the wrong TCP port."))
			}
		} else if totalCount.Unparsed > 0 {
			printer.Stderr.Infof("Captured %d TCP packets total; %d unparsed TCP segments.\n",
				totalCount.TCPPackets, totalCount.Unparsed)
			printer.Stderr.Infof("%s\n", printer.Color.Yellow("This may mean you are trying to capture HTTPS traffic."))
			printer.Stderr.Infof("See https://docs.akita.software/docs/proxy-for-encrypted-traffic\n")
			printer.Stderr.Infof("for instructions on using a proxy, or generate a HAR file with\n")
			printer.Stderr.Infof("your browser as described in\n")
			printer.Stderr.Infof("https://docs.akita.software/docs/collect-client-side-traffic-2\n")
		} else if numUserFilters > 0 && prefilterSummary.Total().HTTPRequests != 0 {
			printer.Stderr.Infof("Captured %d HTTP requests before allow and exclude rules, but all were filtered.\n",
				prefilterSummary.Total().HTTPRequests)
		}
		printer.Stderr.Errorf("%s 🛑\n\n", printer.Color.Red("No HTTP calls captured!"))
		return errors.New("API trace is empty")
	}
	if totalCount.HTTPRequests == 0 {
		printer.Stderr.Warningf("%s ⚠\n\n", printer.Color.Yellow("Saw HTTP responses, but not requests."))
		return nil
	}
	if totalCount.HTTPResponses == 0 {
		printer.Stderr.Warningf("%s ⚠\n\n", printer.Color.Yellow("Saw HTTP requests, but not responses."))
		return nil
	}

	printer.Stderr.Infof("%s 🎉\n\n", printer.Color.Green("Success!"))
	return nil
}

func createLocalCollector(interfaceName, outDir string, tags map[tags.Key]string) (trace.Collector, error) {
	if fi, err := os.Stat(outDir); err == nil {
		// File exists, check if it's a directory.
		if !fi.IsDir() {
			return nil, errors.Errorf("%s is not a directory", outDir)
		}

		// Check if we have permission to write to the directory.
		testFile := filepath.Join(outDir, "akita_test")
		if err := ioutil.WriteFile(testFile, []byte{1}, 0644); err == nil {
			os.Remove(testFile)
		} else {
			return nil, errors.Wrapf(err, "cannot access directory %s", outDir)
		}
	} else {
		// Attempt to create one to make sure there's no permission problem.
		if err := os.Mkdir(outDir, 0755); err != nil {
			return nil, errors.Wrapf(err, "failed to create directory %s", outDir)
		}
	}

	return trace.NewHARCollector(interfaceName, outDir, tags), nil
}
