package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cheggaaa/pb/v3"
	"github.com/cognusion/dnscache"
	"github.com/cognusion/go-humanity"
	"github.com/cognusion/go-racket"
	"github.com/cognusion/go-signalhandler"
	"github.com/cognusion/semaphore"
	"github.com/fatih/color"
	"github.com/rcrowley/go-metrics"
	"github.com/spf13/pflag"
)

// Various globals
var (
	MaxRequests   int           // maximum number of outstanding HTTP get requests allowed
	Rounds        int           // How many times to hit it
	ErrOnly       bool          // Quiet unless 0 == Code >= 400
	NoColor       bool          // Disable colorizing
	NoDNSCache    bool          // Disable DNS caching
	Summary       bool          // Output final stats
	useBar        bool          // Use progress bar
	totalGuess    int           // Guesstimate of number of GETs (useful with -bar)
	debug         bool          // Enable debugging
	ResponseDebug bool          // Enable full response output if debug
	Timeout       time.Duration // How long each GET request may take
	COMPARE       bool          // Are we looking for X-Request-ID headers?

	OutFormat = log.Ldate | log.Ltime | log.Lshortfile
	DebugOut  = log.New(io.Discard, "[DEBUG] ", OutFormat)

	Meter = metrics.NewMeter()

	wchan = make(chan racket.Work)
)

type urlCode struct {
	URL  string
	Code int
	Size int64
	Dur  time.Duration
	Err  error
}

// written to exclusively by collate().
// Unsafe to read from until rChan is closed.
type stat struct {
	count      int
	error4s    int
	error5s    int
	errors     int
	mismatches int
}

func init() {
	pflag.IntVar(&MaxRequests, "max", 5, "Maximium in-flight GET requests at a time")
	pflag.IntVar(&Rounds, "rounds", 100, "Number of times to hit the URL(s)")
	pflag.BoolVar(&ErrOnly, "errorsonly", false, "Only output errors (HTTP Codes >= 400)")
	pflag.BoolVar(&NoColor, "nocolor", false, "Don't colorize the output")
	pflag.BoolVar(&Summary, "stats", false, "Output stats at the end")
	pflag.DurationVar(&Timeout, "timeout", 10*time.Second, "Amount of time to allow each GET request (e.g. 30s, 5m)")
	pflag.BoolVar(&debug, "debug", false, "Enable debug output")
	pflag.BoolVar(&ResponseDebug, "responsedebug", false, "Enable full response output if debugging is on")
	pflag.BoolVar(&NoDNSCache, "nodnscache", false, "Disable DNS caching")
	pflag.BoolVar(&useBar, "bar", false, "Use progress bar instead of printing lines, can still use --stats")
	pflag.IntVar(&totalGuess, "guess", 0, "Rough guess of how many GETs will be coming for --bar to start at. It will adjust")
	pflag.BoolVar(&COMPARE, "checkrequestheader", false, "Checks to see if the response has an X-Request-ID header, else mismatch")

	pflag.CommandLine.MarkHidden("checkrequestheader") // super niche
	pflag.Parse()

	// Handle boring people
	if NoColor {
		color.NoColor = true
	}

	// Handle debug
	if debug {
		DebugOut = log.New(os.Stderr, "[DEBUG] ", OutFormat)
	}

	// Sets the default http client to use dnscache, because duh
	if !NoDNSCache {
		res := dnscache.New(1 * time.Hour)
		http.DefaultClient.Transport = &http.Transport{
			MaxIdleConnsPerHost: 64,
			Dial: func(network string, address string) (net.Conn, error) {
				separator := strings.LastIndex(address, ":")
				ip, _ := res.FetchOneString(address[:separator])
				return net.Dial("tcp", ip+address[separator:])
			},
		}
	}

	// Supervisor uses a semaphore to coordinate the number of running workers.
	// semaphore's Until has a micro-timer to prevent ghostlocks, that defaults
	// to 2ms, which is generous for most operations, however we have more latency
	// in our select{}'s and need to jack it up if MaxRequests is unreasonable.
	semaphore.UntilFreeTimeout = 10 * time.Millisecond
}

func main() {

	var (
		bar       *pb.ProgressBar
		rStats    stat
		rChan     = make(chan urlCode) // Channel to stream responses from the Gets
		abortChan = make(chan bool)    // Channel to tell the getters to abort
	)
	defer Meter.Stop()

	// Set up the progress bar
	if useBar {
		color.Output = io.Discard // disable colorized output
		tmpl := `{{string . "prefix"}}{{counters . }} {{bar . }} {{percent . }} {{rtime . "ETA %s"}}{{string . "suffix"}}`
		bar = pb.ProgressBarTemplate(tmpl).New(totalGuess)
	}

	// Signal handler for SIGINT and SIGKILL
	sigdone := signalhandler.Simple(func(os.Signal) {
		DebugOut.Println("Signal seen, sending abort!")
		close(abortChan)
	})
	defer sigdone()

	if useBar {
		bar.Start()
	}

	// Create the job
	job := racket.NewJob(workerfunc)

	// Hire the supervisor to keep the workers busy
	pchan, doneF := job.Supervisor(MaxRequests, wchan)
	defer close(pchan)

	done := func() {
		DebugOut.Println("Calling done() from Supervisor.")
		doneF()
	}

	go racket.ProgressLogger(DebugOut, true, nil, pchan, nil)

	// Hire a collator to collate the results
	go collate(&rStats, abortChan, rChan, bar)

	// spawn off the scanner to generate Work
	doneChan := make(chan struct{})
	start := time.Now()
	go scanStdIn(wchan, abortChan, rChan, done, doneChan, bar)

	// Wait until all of the work has been assigned.
	<-doneChan
	DebugOut.Println("All work assigned.")

	// we need to wait until all work is assigned (doneChan is closed),
	// and all workers believe they are done (job.IsDone returns)
	// OR abortChan is closed, and it's time to bail.
	select {
	case <-job.IsDone():
		DebugOut.Println("All workers complete.")

	case <-abortChan:
		DebugOut.Println("Abort triggered. Exitting.")
		os.Exit(1)
	}

	// Mop up
	elapsed := time.Since(start)
	close(rChan)

	if useBar {
		bar.Finish()
	}

	if Summary {
		color.Output = os.Stdout // re-enable this
		e := color.RedString("%d", rStats.errors)
		e4 := color.YellowString("%d", rStats.error4s)
		e5 := color.RedString("%d", rStats.error5s)
		eX := color.RedString("%d", rStats.mismatches)
		fmt.Printf("\n\nGETs: %d\nErrors: %s\n500 Errors: %s\n400 Errors: %s\nMismatches: %s\nElapsed Time: %s\nRate: %.4f/s\n", rStats.count, e, e5, e4, eX, elapsed.String(), Meter.RateMean())
	}
}

func collate(stats *stat, abortChan chan bool, rChan chan urlCode, bar *pb.ProgressBar) {
	defer DebugOut.Println("collate complete")

	var zu = urlCode{}
	for i := range rChan {
		if i == zu {
			// zero value means we're done
			return
		}

		// check for
		select {
		case <-abortChan:
			// Aborting
			DebugOut.Println("collate aborting!")
			return
		default:
		}

		stats.count++

		if bar != nil {
			bar.Increment()
		}

		if i.Code == 0 {
			stats.errors++
			color.Red("%d (%s) %s %s (%s)\n", i.Code, humanity.ByteFormat(i.Size), i.URL, i.Dur.String(), i.Err)
		} else if i.Code < 400 {
			if ErrOnly {
				// skip
				continue
			}
			color.Green("%d (%s) %s %s\n", i.Code, humanity.ByteFormat(i.Size), i.URL, i.Dur.String())
		} else if i.Code < 500 {
			stats.error4s++
			color.Yellow("%d (%s) %s %s\n", i.Code, humanity.ByteFormat(i.Size), i.URL, i.Dur.String())
		} else if i.Code < 600 {
			stats.error5s++
			color.Red("%d (%s) %s %s\n", i.Code, humanity.ByteFormat(i.Size), i.URL, i.Dur.String())
		} else {
			stats.mismatches++
			color.Red("%d (%s) %s %s\n", i.Code, humanity.ByteFormat(i.Size), i.URL, i.Dur.String())
		}
	}
}

func workerfunc(id any, work racket.Work, pchan chan<- racket.Progress) {
	//defer func() { pchan <- racket.PMessagef("[WORKER %v] Done.", id) }()

	var (
		url              = work.GetString("url")
		rChan            = work.Get("rChan").(chan urlCode)
		compareRequestID = work.GetBool("compare")
		timeout          = work.Get("timeout").(time.Duration)
		ctx              = context.Background()
		cancel           context.CancelFunc
	)

	pchan <- racket.PMessagef("[WORKER %v] Doing %+v.", id, work)

	// Create the context
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// GET!
	c := http.DefaultClient // this may have been customized previously, to cache DNS requests
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		// We assume code 0 to be a non-HTTP error
		pchan <- racket.PErrorf("[WORKER %v] Error during request creation: %w", id, err)
		rChan <- urlCode{url, 0, 0, 0, err}
	}

	s := time.Now()
	response, err := c.Do(req)
	d := time.Since(s)
	Meter.Mark(1)

	if err != nil {
		// We assume code 0 to be a non-HTTP error
		pchan <- racket.PErrorf("[WORKER %v] Error during HTTP request: %w", id, err)
		rChan <- urlCode{url, 0, 0, d, err}
	} else {
		defer response.Body.Close()

		if ResponseDebug {
			b, err := io.ReadAll(response.Body)
			if err != nil {
				DebugOut.Printf("Error reading response body: %s\n", err)
			} else {
				DebugOut.Printf("<-----\n%s\n----->\n", b)
			}
		}

		if compareRequestID {
			if cv := compare(response); !cv {
				rChan <- urlCode{url, 611, response.ContentLength, d, nil}
				return
			}
		}
		//pchan <- racket.PMessagef("[WORKER %v] Sending final result.", id)
		rChan <- urlCode{url, response.StatusCode, response.ContentLength, d, nil}
	}
}

// scanStdIn takes a channel to pass inputted strings to,
// and does so until EOF, whereafter it closes the channel
func scanStdIn(workChan chan racket.Work, abortChan chan bool, rChan chan urlCode, done func(), doneChan chan struct{}, bar *pb.ProgressBar) {
	defer done()
	defer close(doneChan)

	var count int64
	var lines int64
	defer func() { DebugOut.Printf("EOF seen after %d lines. %d total work\n", lines, count) }()
	scanner := bufio.NewScanner(os.Stdin)
	for {
		select {
		case <-abortChan:
			DebugOut.Println("scanner aborting!")
			return
		case b := <-boolWaiter(scanner.Scan):
			// woo!
			if !b {
				// We're done
				return
			}
		}
		DebugOut.Println("scanner sending...")
		lines++

		getURL := scanner.Text()
		count += int64(Rounds)

		// Update bar total
		if bar != nil && bar.Total() < count {
			bar.SetTotal(count)
		}

		// if Rounds is reasonable, the select inside is smoothing a smooth rug,
		// but if Rounds is unreasonable, it prevents a nasty trip hazard.
		for range Rounds {
			select {
			case <-abortChan:
				DebugOut.Println("scanner aborting inside assignment!")
				return
			case workChan <- racket.NewWork(map[string]any{
				"url":     getURL,
				"rChan":   rChan,
				"compare": COMPARE,
				"timeout": Timeout,
			}):
				// Work assigned
			}
		}
	}
}

func compare(resp *http.Response) bool {
	reqid := resp.Header.Get("X-Request-ID")
	reader := bufio.NewReader(resp.Body)
	line, _ := reader.ReadString(' ')
	line = strings.TrimSpace(line)
	DebugOut.Printf("%s == %s?\n", reqid, line)
	return line == reqid
}

func boolWaiter(f func() bool) <-chan bool {
	var bchan = make(chan bool, 1)

	go func() { bchan <- f() }()

	return bchan
}
