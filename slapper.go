package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
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

	rateIncreaseStep = 100
	rateDecreaseStep = 100
)

var (
	requestsSent      counter
	responsesReceived counter
	responses         [1024]counter

	timingsOk  [][]counter
	timingsBad [][]counter

	terminalWidth  uint
	terminalHeight uint

	// plotting vars
	plotWidth  int
	plotHeight int
	buckets    int
	logBase    float64
	minY, maxY float64
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
	if targets == "stdin" {
		reader = bufio.NewReader(os.Stdin)
	} else {
		f, err := os.Open(targets)
		if err != nil {
			return nil, err
		}

		reader = bufio.NewReader(f)
		defer f.Close()
	}

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
			b = []byte("")
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
		string(st.url),
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

				tOk, tBad := getTimingsSlot(now)

				elapsed := now.Sub(start)
				elapsedMs := float64(elapsed) / float64(time.Millisecond)
				elapsedBucket := int(math.Log(elapsedMs) / math.Log(logBase))
				if elapsedBucket >= len(tOk) {
					elapsedBucket = len(tOk) - 1
				} else if elapsedBucket < 0 {
					elapsedBucket = 0
				}

				responsesReceived.Add(1)

				var status int
				if err == nil {
					status = response.StatusCode
				} else {
					status = 700
				}

				responses[status].Add(1)
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
	barWidth := plotWidth - 40 // reserve some space on right and left

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
			fmt.Printf("\033[96mrate: %4d RPS\033[0m ", currentRate.Load())

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
			fmt.Print("\n\n")

			width := float64(barWidth) / float64(max)
			for bkt := 0; bkt < buckets; bkt++ {
				startMs := math.Pow(logBase, float64(bkt))
				endMs := math.Pow(logBase, float64(bkt)+1)

				var label string
				if bkt == buckets-1 {
					label = fmt.Sprintf("%3.0f+", startMs)
				} else if endMs > 10 {
					label = fmt.Sprintf("%3.0f-%3.0f", startMs, endMs)
				} else {
					label = fmt.Sprintf("%.1f-%.1f", startMs, endMs)
				}

				widthOk := int(float64(tOk[bkt]) * width)
				widthBad := int(float64(tBad[bkt]) * width)
				widthLeft := barWidth - widthOk - widthBad

				fmt.Printf("%10s ms: [%s%6d%s/%s%6d%s] %s%s%s%s%s \n",
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

func keyPressListener(increase, decrease chan<- int) {
	// start keyPress listener
	err := term.Init()
	if err != nil {
		panic(err)
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
					increase <- rateIncreaseStep
				case 'j':
					decrease <- rateDecreaseStep
				}
			}
		case term.EventError:
			panic(ev.Err)
		}
	}
}

func ticker(rate int, quit <-chan struct{}) (<-chan time.Time, chan<- int, chan<- int) {
	ticker := make(chan time.Time, 1)
	increase := make(chan int, 1)
	decrease := make(chan int, 1)

	// start main workers
	go func() {
		currentRate := rate
		tck := time.Tick(time.Duration(1e9 / currentRate))

		for {
			select {
			case v := <-increase:
				currentRate += v
				tck = time.Tick(time.Duration(1e9 / currentRate))
			case v := <-decrease:
				currentRate -= v
				tck = time.Tick(time.Duration(1e9 / currentRate))
			case t := <-tck:
				ticker <- t
			case <-quit:
				return
			}
		}
	}()

	return ticker, increase, decrease
}

func getTimingsSlot(now time.Time) ([]counter, []counter) {
	n := int(now.UnixNano() / 100000000)
	slot := n % len(timingsOk)
	return timingsOk[slot], timingsBad[slot]
}

func initializeTimingsBucket(buckets int) {
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
			tOk, tBad := getTimingsSlot(now)
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
	workers := flag.Int("workers", 8, "Number of workers")
	timeout := flag.Duration("timeout", 30*time.Second, "Requests timeout")
	targets := flag.String("targets", "stdin", "Targets file")
	rate := flag.Int("rate", 50, "Requests per second)")
	miY := flag.Duration("minY", time.Millisecond, "min on Y axe")
	maY := flag.Duration("maxY", 100*time.Millisecond, "max on Y axe")
	flag.Parse()

	terminalWidth, _ = terminal.Width()
	terminalHeight, _ = terminal.Height()

	plotWidth = int(terminalWidth)
	plotHeight = int(terminalHeight) - statsLines

	minY, maxY = float64(*miY/time.Millisecond), float64(*maY/time.Millisecond)
	deltaY := maxY - minY
	buckets = plotHeight
	logBase = math.Pow(deltaY, 1/float64(buckets))

	initializeTimingsBucket(buckets)

	quit := make(chan struct{}, 1)
	ticker, increase, decrease := ticker(*rate, quit)

	trgt, err := newTargeter(*targets)
	if err != nil {
		panic(err)
	}

	// start attackers
	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
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

	keyPressListener(increase, decrease)

	// bye
	close(quit)
	wg.Wait()
}
