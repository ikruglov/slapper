package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	term "github.com/nsf/termbox-go"
	terminal "github.com/wayneashleyberry/terminal-dimensions"
)

const (
	statsLines             = 3
	movingWindowsSize      = 10 // seconds
	screenRefreshFrequency = 10 // per second
	screenRefreshInterval  = time.Second / screenRefreshFrequency

	reservedWidthSpace  = 40
	reservedHeightSpace = 3

	rateIncreaseStep = 100
	rateDecreaseStep = -100
)

var (
	requestsSent      counter
	responsesReceived counter
	responses         [1024]counter
	desiredRate       counter

	timingsOk  [][]counter
	timingsBad [][]counter

	terminalWidth  uint
	terminalHeight uint

	// plotting vars
	plotWidth  uint
	plotHeight uint

	// first bucket is for requests faster then minY,
	// last of for ones slower then maxY
	buckets    uint
	logBase    float64
	minY, maxY float64
	startMs    float64
)

func resetStats() {
	requestsSent.Store(0)
	responsesReceived.Store(0)

	for _, ok := range timingsOk {
		for i := 0; i < len(ok); i++ {
			ok[i].Store(0)
		}
	}

	for _, bad := range timingsBad {
		for i := 0; i < len(bad); i++ {
			bad[i].Store(0)
		}
	}

	for i := 0; i < len(responses); i++ {
		responses[i].Store(0)
	}
}

type counter int64

func (c *counter) Add(v int64) int64 { return atomic.AddInt64((*int64)(c), v) }
func (c *counter) Load() int64       { return atomic.LoadInt64((*int64)(c)) }
func (c *counter) Store(v int64)     { atomic.StoreInt64((*int64)(c), v) }

type targeter struct {
	idx      counter
	requests []struct {
		method string
		url    string
		body   []byte
	}
}

func newTargeter(targets string) (*targeter, error) {
	var reader *bufio.Reader
	f, err := os.Open(targets)
	if err != nil {
		return nil, err
	}

	reader = bufio.NewReader(f)
	defer f.Close()

	trgt := &targeter{}

	// syntax
	// GET <url>\n
	// $ <body>\n
	// \n

	for {
		var method string
		var url, body []byte

		b, err := reader.ReadBytes('\n')
		if err != nil {
			break
		}

		b = bytes.Trim(b, " \t\n")
		if !bytes.HasPrefix(b, []byte("GET")) {
			continue
		}

		method = http.MethodGet
		url = bytes.TrimLeft(b[3:], " \t\n")

		b, err = reader.ReadBytes('\n')
		if err != nil {
			break
		}

		b = bytes.Trim(b, " \t\n")
		if len(b) == 0 {
			body = []byte("")
		} else if bytes.HasPrefix(b, []byte("$ ")) {
			body = b[2:]

			b, err = reader.ReadBytes('\n')
			if err != nil {
				break
			}

			if b = bytes.Trim(b, " \t\n"); len(b) != 0 {
				continue
			}
		}

		trgt.requests = append(trgt.requests, struct {
			method string
			url    string
			body   []byte
		}{
			method: method,
			url:    string(url),
			body:   body,
		})
	}

	return trgt, nil
}

func (t *targeter) nextRequest() (*http.Request, error) {
	if len(t.requests) == 0 {
		return nil, errors.New("no requests")
	}

	idx := int(t.idx.Add(1))
	st := t.requests[idx%len(t.requests)]
	return http.NewRequest(
		st.method,
		st.url,
		bytes.NewReader(st.body),
	)
}

func attack(trgt *targeter, timeout time.Duration, ch <-chan time.Time, quit <-chan struct{}) {
	tr := &http.Transport{
		DisableKeepAlives:   false,
		DisableCompression:  true,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     30 * time.Second,
	}

	client := &http.Client{
		Transport: tr,
		Timeout:   timeout,
	}

	for {
		select {
		case <-ch:
			if request, err := trgt.nextRequest(); err == nil {
				requestsSent.Add(1)

				start := time.Now()
				response, err := client.Do(request)
				if err == nil {
					_, err = ioutil.ReadAll(response.Body)
					response.Body.Close()
				}
				now := time.Now()

				elapsed := now.Sub(start)
				elapsedMs := float64(elapsed) / float64(time.Millisecond)
				correctedElapsedMs := elapsedMs - startMs
				elapsedBucket := int(math.Log(correctedElapsedMs) / math.Log(logBase))

				// first bucket is for requests faster then minY,
				// last of for ones slower then maxY
				if elapsedBucket < 0 {
					elapsedBucket = 0
				} else if elapsedBucket >= int(buckets)-1 {
					elapsedBucket = int(buckets) - 1
				} else {
					elapsedBucket = elapsedBucket + 1
				}

				responsesReceived.Add(1)

				status := 0
				if err == nil {
					status = response.StatusCode
				}

				responses[status].Add(1)
				tOk, tBad := getTimingsSlot(now)
				if status >= 200 && status < 300 {
					tOk[elapsedBucket].Add(1)
				} else {
					tBad[elapsedBucket].Add(1)
				}
			}
		case <-quit:
			return
		}
	}
}

func reporter(quit <-chan struct{}) {
	fmt.Print("\033[H")
	for i := 0; i < int(terminalHeight); i++ {
		fmt.Println(string(bytes.Repeat([]byte(" "), int(terminalWidth)-1)))
	}

	var currentRate counter
	go func() {
		var lastSent int64
		for _ = range time.Tick(time.Second) {
			curr := requestsSent.Load()
			currentRate.Store(curr - lastSent)
			lastSent = curr
		}
	}()

	colors := []string{
		"\033[38;5;46m", "\033[38;5;47m", "\033[38;5;48m", "\033[38;5;49m", // green
		"\033[38;5;149m", "\033[38;5;148m", "\033[38;5;179m", "\033[38;5;176m", // yellow
		"\033[38;5;169m", "\033[38;5;168m", "\033[38;5;197m", "\033[38;5;196m", // red
	}

	colorMultiplier := float64(len(colors)) / float64(buckets)
	barWidth := int(plotWidth) - reservedWidthSpace // reserve some space on right and left

	ticker := time.Tick(screenRefreshInterval)
	for {
		select {
		case <-ticker:
			// scratch arrays
			tOk := make([]int64, len(timingsOk))
			tBad := make([]int64, len(timingsBad))

			// need to understand how long in longest bar,
			// also take a change to copy arrays to have consistent view

			max := int64(1)
			for i := 0; i < len(timingsOk); i++ {
				ok := timingsOk[i]
				bad := timingsBad[i]

				for j := 0; j < len(ok); j++ {
					tOk[j] += ok[j].Load()
					tBad[j] += bad[j].Load()
					if sum := tOk[j] + tBad[j]; sum > max {
						max = sum
					}
				}
			}

			sent := requestsSent.Load()
			recv := responsesReceived.Load()
			fmt.Print("\033[H") // clean screen
			fmt.Printf("sent: %-6d ", sent)
			fmt.Printf("in-flight: %-2d ", sent-recv)
			fmt.Printf("\033[96mrate: %4d/%d RPS\033[0m ", currentRate.Load(), desiredRate.Load())

			fmt.Print("responses: ")
			for status, counter := range responses {
				if c := counter.Load(); c > 0 {
					if status >= 200 && status < 300 {
						fmt.Printf("\033[32m[%d]: %-6d\033[0m ", status, c)
					} else {
						fmt.Printf("\033[31m[%d]: %-6d\033[0m ", status, c)
					}
				}
			}
			fmt.Print("\r\n\r\n")

			width := float64(barWidth) / float64(max)
			for bkt := uint(0); bkt < buckets; bkt++ {
				var label string
				if bkt == 0 {
					if startMs >= 10 {
						label = fmt.Sprintf("<%.0f", startMs)
					} else {
						label = fmt.Sprintf("<%.1f", startMs)
					}
				} else if bkt == buckets-1 {
					if maxY >= 10 {
						label = fmt.Sprintf("%3.0f+", maxY)
					} else {
						label = fmt.Sprintf("%.1f+", maxY)
					}
				} else {
					beginMs := minY + math.Pow(logBase, float64(bkt-1))
					endMs := minY + math.Pow(logBase, float64(bkt))

					if endMs >= 10 {
						label = fmt.Sprintf("%3.0f-%3.0f", beginMs, endMs)
					} else {
						label = fmt.Sprintf("%.1f-%.1f", beginMs, endMs)
					}
				}

				widthOk := int(float64(tOk[bkt]) * width)
				widthBad := int(float64(tBad[bkt]) * width)
				widthLeft := barWidth - widthOk - widthBad

				fmt.Printf("%10s ms: [%s%6d%s/%s%6d%s] %s%s%s%s%s \r\n",
					label,
					"\033[32m",
					tOk[bkt],
					"\033[0m",
					"\033[31m",
					tBad[bkt],
					"\033[0m",
					colors[int(float64(bkt)*colorMultiplier)],
					bytes.Repeat([]byte("E"), widthBad),
					bytes.Repeat([]byte("*"), widthOk),
					bytes.Repeat([]byte(" "), widthLeft),
					"\033[0m")
			}
		case <-quit:
			return
		}
	}
}

func keyPressListener(rateChanger chan<- int64) {
	// start keyPress listener
	err := term.Init()
	if err != nil {
		log.Fatal(err)
	}

	defer term.Close()

keyPressListenerLoop:
	for {
		switch ev := term.PollEvent(); ev.Type {
		case term.EventKey:
			switch ev.Key {
			case term.KeyCtrlC:
				break keyPressListenerLoop
			default:
				switch ev.Ch {
				case 'q':
					break keyPressListenerLoop
				case 'r':
					resetStats()
				case 'k': // up
					rateChanger <- rateIncreaseStep
				case 'j':
					rateChanger <- rateDecreaseStep
				}
			}
		case term.EventError:
			log.Fatal(ev.Err)
		}
	}
}

func ticker(rate uint64, quit <-chan struct{}) (<-chan time.Time, chan<- int64) {
	ticker := make(chan time.Time, 1)
	rateChanger := make(chan int64, 1)

	// start main workers
	go func() {
		desiredRate.Store(int64(rate))
		tck := time.NewTicker(time.Duration(1e9 / rate))

		for {
			select {
			case r := <-rateChanger:
				tck.Stop()
				if newRate := desiredRate.Add(r); newRate > 0 {
					tck = time.NewTicker(time.Duration(1e9 / newRate))
				} else {
					desiredRate.Store(0)
				}
			case t := <-tck.C:
				ticker <- t
			case <-quit:
				return
			}
		}
	}()

	return ticker, rateChanger
}

func getTimingsSlot(now time.Time) ([]counter, []counter) {
	n := int(now.UnixNano() / 100000000)
	slot := n % len(timingsOk)
	return timingsOk[slot], timingsBad[slot]
}

func initializeTimingsBucket(buckets uint) {
	timingsOk = make([][]counter, movingWindowsSize*screenRefreshFrequency)
	for i := 0; i < len(timingsOk); i++ {
		timingsOk[i] = make([]counter, buckets)
	}

	timingsBad = make([][]counter, movingWindowsSize*screenRefreshFrequency)
	for i := 0; i < len(timingsBad); i++ {
		timingsBad[i] = make([]counter, buckets)
	}

	go func() {
		for now := range time.Tick(screenRefreshInterval) {
			// TODO account for missing ticks
			// clean next timing slot which is last one in ring buffer
			next := now.Add(screenRefreshInterval)
			tOk, tBad := getTimingsSlot(next)
			for i := 0; i < len(tOk); i++ {
				tOk[i].Store(0)
			}

			for i := 0; i < len(tBad); i++ {
				tBad[i].Store(0)
			}
		}
	}()
}

func main() {
	workers := flag.Uint("workers", 8, "Number of workers")
	timeout := flag.Duration("timeout", 30*time.Second, "Requests timeout")
	targets := flag.String("targets", "", "Targets file")
	rate := flag.Uint64("rate", 50, "Requests per second")
	miY := flag.Duration("minY", 0, "min on Y axe (default 0ms)")
	maY := flag.Duration("maxY", 100*time.Millisecond, "max on Y axe")
	flag.Parse()

	terminalWidth, _ = terminal.Width()
	terminalHeight, _ = terminal.Height()

	plotWidth = terminalWidth
	plotHeight = terminalHeight - statsLines

	if plotWidth <= reservedWidthSpace {
		log.Fatal("not enough screen width, min 40 characters required")
	}

	if plotHeight <= reservedHeightSpace {
		log.Fatal("not enough screen height, min 3 lines required")
	}

	minY, maxY = float64(*miY/time.Millisecond), float64(*maY/time.Millisecond)
	deltaY := maxY - minY
	buckets = plotHeight
	logBase = math.Pow(deltaY, 1/float64(buckets-2))
	startMs = minY + math.Pow(logBase, 0)

	initializeTimingsBucket(buckets)

	quit := make(chan struct{}, 1)
	ticker, rateChanger := ticker(*rate, quit)

	trgt, err := newTargeter(*targets)
	if err != nil {
		log.Fatal(err)
	}

	// start attackers
	var wg sync.WaitGroup
	for i := uint(0); i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			attack(trgt, *timeout, ticker, quit)
		}()
	}

	// start reporter
	wg.Add(1)
	go func() {
		defer wg.Done()
		reporter(quit)
	}()

	keyPressListener(rateChanger)

	// bye
	close(quit)
	wg.Wait()
}
