// The MIT License (MIT)
//
// Copyright (c) 2014 Jamie Alquiza
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	kafka "github.com/jamiealquiza/sangrenel/vendor/github.com/Shopify/sarama"
)

var (
	// Configs.
	brokers       []string
	topic         string
	msgSize       int
	msgRate       int64
	clientWorkers int
	noop          bool

	// Character selection from which random messages are generated.
	chars = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890!@#$%^&*(){}][:<>.")

	// Counters / misc.
	sig_chan        = make(chan os.Signal)
	clientKill_chan = make(chan bool, 24)
	sentCntr        = make(chan int64, 1)
	latency         []float64
	latency_chan    = make(chan float64, 1)
	resetLat_chan   = make(chan bool, 1)
)

func init() {
	flag.StringVar(&topic, "topic", "sangrenel", "Topic to publish to")
	flag.IntVar(&msgSize, "size", 300, "Message size in bytes")
	flag.Int64Var(&msgRate, "rate", 100000000, "Apply a global message rate limit")
	flag.BoolVar(&noop, "noop", false, "Test message generation performance, do not transmit messages")
	flag.IntVar(&clientWorkers, "workers", 1, "Number of Kafka client workers")
	brokerString := flag.String("brokers", "localhost:9092", "Comma delimited list of Kafka brokers")
	flag.Parse()

	brokers = strings.Split(*brokerString, ",")

	sentCntr <- 0
	runtime.GOMAXPROCS(runtime.NumCPU())
}

// clientWorker generates random messages and writes to Kafka.
// Workers track and limit message rates using incrSent() and fetchSent().
// 5 instances of clientWorker are created under each Kafka client.
func clientWorker(c kafka.Client) {
	producer, err := kafka.NewSimpleProducer(&c, nil)
	if err != nil {
		fmt.Println(err.Error())
	}
	defer producer.Close()

	// Instantiate rand per producer to avoid mutex contention.
	source := rand.NewSource(time.Now().UnixNano())
	generator := rand.New(source)
	// Create msg object once, reuse.
	msg := make([]rune, msgSize)

	// Use a local accumulator then periodically update global counter.
	// Global counter can become a bottleneck with too many threads.
	tick := time.Tick(1 * time.Second)
	var n int64

	for {
		// Message rate limit works by having all clientWorker loops incrementing
		// a global counter and tracking the aggregate per-second progress.
		// If the configured rate is met, the worker will sleep
		// for the remainder of the 1 second window.
		rateEnd := time.Now().Add(time.Second)
		countStart := fetchSent()
		var start time.Time
		for fetchSent()-countStart < msgRate {
			data := randMsg(msg, *generator)
			// We start timing after the message is created.
			// This ensures latency metering from the time between message sent and receiving an ack.
			start = time.Now()
			err = producer.SendMessage(topic, nil, kafka.StringEncoder(data))
			if err != nil {
				fmt.Println(err)
			} else {
				// Increment global sent count and fire off time since start value into the latency channel.
				n++
				select {
				case <-tick:
					incrSent(n)
					n = 0
				default:
					break
				}
				latency_chan <- time.Since(start).Seconds() * 1000
			}
		}
		// If the global per-second rate limit was met,
		// the inner loop breaks and the outer loop sleeps for the second remainder.
		time.Sleep(rateEnd.Sub(time.Now()) + time.Since(start))
	}
}

// kafkaClient initializes a connection to a Kafka cluster.
// Each client manages 5 clientWorker() (producer instances).
// Fixed value since this seem to flatline throughput at ~5.
func kafkaClient(n int) {
	switch noop {
	// If not noop, actually fire up Kafka connections and send messages.
	case false:
		cId := "client_" + strconv.Itoa(n)
		client, err := kafka.NewClient(cId, brokers, kafka.NewClientConfig())
		if err != nil {
			panic(err)
		} else {
			fmt.Printf("%s connected\n", cId)
		}
		for i := 0; i < 5; i++ {
			go clientWorker(*client)
		}
	// If noop, we're not creating connections at all.
	// Just generate messages and burn CPU.
	default:
		for i := 0; i < 5; i++ {
			go genMsgs()
		}
	}
	<-clientKill_chan
}

// genMsgs is a dummy function that kafkaClient calls if noop is True.
// It is used in place of starting actual Kafka client connections to test message creation performance.
func genMsgs() {
	// Instantiate 'rand' per producer to avoid mutex contention.
	source := rand.NewSource(time.Now().UnixNano())
	generator := rand.New(source)
	// Define msg objet once, reuse.
	msg := make([]rune, msgSize)

	tick := time.Tick(1 * time.Second)
	var n int64

	for {
		// Generate a random message and increment the global counter.
		randMsg(msg, *generator)
		n++
		select {
		case <-tick:
			incrSent(n)
			n = 0
		default:
			break
		}
	}
}

// Returns a random message generated from the chars rune slice.
// Message length of m bytes as defined by msgSize.
func randMsg(m []rune, generator rand.Rand) string {
	for i := range m {
		m[i] = chars[generator.Intn(len(chars))]
	}
	return string(m)
}

// Thread-safe global counter functions.
func incrSent(n int64) {
	i := <-sentCntr
	sentCntr <- i + n
}
func fetchSent() int64 {
	i := <-sentCntr
	sentCntr <- i
	return i
}

// Thread-safe receiver for latency values captured by all producer goroutines.
// May want to do something smart about this to limit time to sort
// huge slices in high-throughput configurations where lots of latency values are received.
func latencyAggregator() {
	for {
		select {
		case i := <-latency_chan:
			latency = append(latency, i)
		case <-resetLat_chan:
			latency = latency[:0]
		}
	}
}

// Calculates aggregate raw message output in networking friendly units.
// Gives an idea of minimum network traffic being generated.
func calcOutput(n int64) string {
	m := (float64(n) / 5) * float64(msgSize)
	var o string
	switch {
	case m >= 131072:
		o = strconv.FormatFloat(m/131072, 'f', 0, 64) + "Mb/sec"
	case m < 131072:
		o = strconv.FormatFloat(m/1024, 'f', 0, 64) + "KB/sec"
	}
	return o
}

// Fetches & resets current latencies set held by 'latencyAggregator()'.
// Sorts then averages the 90th percentile worse latencies.
func calcLatency() float64 {
	var avg float64
	// With 'noop', we don't have latencies to operate on.
	switch noop {
	case true:
		break
	default:
		// Fetch values.
		lat := latency
		// Issue the current values to be cleared.
		resetLat_chan <- true
		// Sort and sum values.
		sort.Float64s(lat)
		var sum float64
		// Get percentile count and values, sum values.
		topn := int(float64(len(lat)) * 0.90)
		for i := topn; i < len(lat); i++ {
			sum += lat[i]
		}
		// Calc average.
		avg = sum / float64(len(lat)-topn)
	}
	return avg
}

func main() {
	// Listens for signals.
	signal.Notify(sig_chan, syscall.SIGINT, syscall.SIGTERM)
	// Fires up 'latencyAggregator()' to background.
	go latencyAggregator()

	// Print Sangrenel startup info.
	fmt.Printf("\n::: Sangrenel :::\nStarting %s workers\nMessage size %s bytes\n\n",
		strconv.Itoa(clientWorkers),
		strconv.Itoa(msgSize))

	// Start client workers.
	for i := 0; i < clientWorkers; i++ {
		go kafkaClient(i + 1)
	}

	// Start Sangrenel periodic info output.
	tick := time.Tick(5 * time.Second)
	// Count mile-markers for tracking message rates.
	var currCnt, lastCnt int64
	for {
		select {
		case <-tick:
			// Set last and current to last read sent count.
			lastCnt = currCnt
			// Get actual current sent count, then delta from last count.
			// Delta is divided by update interval (5s) for per-second rate over output updates.
			currCnt = fetchSent()
			deltaCnt := currCnt - lastCnt
			fmt.Printf("%s Generating %s @ %d messages/sec | topic: %s | %.2fms avg latency\n",
				// Always be RFC'ing.
				time.Now().Format(time.RFC3339),
				calcOutput(deltaCnt),
				deltaCnt/5,
				topic,
				// Well, this technically appends a small latency to the 5s interval.
				calcLatency())
		// Waits for signals. Currently just brutally kills Sangrenel.
		case <-sig_chan:
			fmt.Println("Killing Connections")
			for i := 0; i < clientWorkers; i++ {
				clientKill_chan <- true
			}
			close(clientKill_chan)
			time.Sleep(2 * time.Second)
			os.Exit(0)
		}
	}
}
