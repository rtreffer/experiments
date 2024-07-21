package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"syscall"
	"time"
	"unicode/utf8"
)

var latencies []time.Duration

func handleSignals(cancel context.CancelFunc) {
	channel := make(chan os.Signal, 1)
	signal.Notify(channel, os.Interrupt, syscall.SIGTERM)
	for range channel {
		cancel()
	}
}

func roundLatency(d time.Duration) time.Duration {
	if d <= time.Millisecond {
		return d
	}
	if d <= time.Second {
		return ((d + 500) / time.Microsecond) * time.Microsecond
	}
	return ((d + 500*time.Microsecond) / time.Millisecond) * time.Millisecond
}

func latencyReport(data []time.Duration) {
	if len(data) == 0 {
		fmt.Println("no latencies to report")
		return
	}

	// create a copy of the data
	latencies := make([]time.Duration, len(data))
	copy(latencies, data)
	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i] < latencies[j]
	})
	p50 := latencies[len(latencies)/2]
	p90 := latencies[len(latencies)*9/10]
	p99 := latencies[len(latencies)*99/100]
	p999 := latencies[len(latencies)*999/1000]
	min := latencies[0]
	max := latencies[len(latencies)-1]
	var sum time.Duration
	for _, d := range latencies {
		sum += d
	}
	avg := sum / time.Duration(len(latencies))
	var variance time.Duration
	for _, d := range latencies {
		diff := d - avg
		variance += diff * diff
	}
	stddev := time.Duration(0)
	if len(latencies) > 1 {
		stddev = time.Duration(math.Sqrt(float64(variance) / float64(len(latencies))))
	}
	fmt.Printf("latency report (%d entries): min=%s p50=%s p90=%s p99=%s p999=%s max=%s avg=%s stddev=%s\n",
		len(latencies), min, p50, p90, p99, p999, max, avg, stddev)

	// 2^32 nanoseconds ~ 4 seconds
	buckets := make([]int, 32)
	bucketBoundary := time.Duration(1)
	index := 0
	for _, d := range latencies {
		for d > bucketBoundary {
			bucketBoundary += time.Duration(1)
			bucketBoundary *= 2
			bucketBoundary -= time.Duration(1)
			index++
		}
		if index >= len(buckets) {
			buckets[len(buckets)-1]++
			continue
		}
		buckets[index]++
	}

	maxCount := 0
	for _, count := range buckets {
		if count > maxCount {
			maxCount = count
		}
	}

	output := false
	buf := ""
	maxStars := 50
	for i, count := range buckets {
		if count == 0 && !output {
			continue
		}
		output = true
		bucketEnd := time.Duration(1)
		for j := 0; j < i; j++ {
			bucketEnd += time.Duration(1)
			bucketEnd *= 2
			bucketEnd -= time.Duration(1)
		}
		bucketStart := (bucketEnd + time.Duration(1)) / 2
		stars := int(float64(count) / float64(maxCount+1) * float64(1+maxStars))
		starsStr := "|"
		for j := 0; j < maxStars; j++ {
			if j < stars {
				starsStr += "*"
			} else {
				starsStr += " "
			}
		}
		starsStr += "|"
		left := roundLatency(bucketStart).String()
		right := roundLatency(bucketEnd).String()
		for utf8.RuneCountInString(left) < 9 {
			left = " " + left
		}
		for utf8.RuneCountInString(right) < 9 {
			right = right + " "
		}
		prefix := fmt.Sprintf("%s -> %s (%7d)", left, right, count)
		if count == 0 {
			buf += fmt.Sprintf("%s %s\n", prefix, starsStr)
			continue
		}
		if buf != "" {
			fmt.Print(buf)
			buf = ""
		}
		fmt.Printf("%s %s\n", prefix, starsStr)
	}
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if runtime.GOMAXPROCS(0) < 2 {
		runtime.GOMAXPROCS(2)
	}

	// handle signals
	go handleSignals(cancel)

	// setup flag parsing:
	// --file string: the file to write to
	// --file.max-size int: the maximum size of the file in bytes
	// --frequency float64: the number of times per second to print to stdout
	// --bytes-per-second int: the number of bytes per second to write to the file
	// --history.length int: the number of latencies to keep in memory
	// --latency.interval int: the number of seconds between latency reports
	file := flag.String("file", "iolat.dat", "the file to write to")
	fileMaxSize := flag.Int("file.max-size", 32*(1<<20), "the maximum size of the file in bytes")
	bytesPerSecond := flag.Int("bytes-per-second", 40*4960, "the number of bytes per second to write to the file")
	frequency := flag.Float64("frequency", 10.0, "the number of times per second to print to stdout")
	historyLength := flag.Int("history.length", 1000, "the number of latencies to keep in memory")
	latencyInterval := flag.Int("latency.interval", 10, "the number of seconds between latency reports")
	flag.Parse()

	tickerTime := time.Duration(1 / *frequency * 1e9)
	latencies = make([]time.Duration, *historyLength)

	// remove old file
	os.Remove(*file)
	// open the file
	fileHandle, err := os.OpenFile(*file, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Println("error opening file:", err)
		return
	}
	// pre generate the data array
	data := make([]byte, *bytesPerSecond)
	for i := 0; i < len(data); i++ {
		data[i] = byte(i % 256)
	}

	written := 0

	lastLatencyTime := time.Now()
	ticker := time.NewTicker(tickerTime)
	cnt := 0
	sliceSize := ((*bytesPerSecond + int(*frequency) - 1) / int(*frequency))
	for {
		select {
		case <-ctx.Done():
			fmt.Println()
			fmt.Println("exiting")
			if cnt < *historyLength {
				latencyReport(latencies[:cnt])
			} else {
				latencyReport(latencies)
			}
			return
		case <-ticker.C:
		}
		startOffset := (cnt % int(*frequency)) * sliceSize
		endOffset := startOffset + sliceSize
		if endOffset > len(data) {
			endOffset = len(data)
		}

		start := time.Now()
		_, err := fileHandle.Write(data[startOffset:endOffset])
		dt := time.Since(start)
		if err != nil {
			fmt.Println("error writing to file:", err)
			return
		}
		written += endOffset - startOffset

		if written >= *fileMaxSize {
			fileHandle.Close()
			os.Remove(*file)
			fileHandle, err = os.OpenFile(*file, os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				fmt.Println("error opening file:", err)
				return
			}
			written = 0
		}

		latencies[cnt%*historyLength] = dt
		cnt++

		if time.Since(lastLatencyTime) > time.Duration(*latencyInterval)*time.Second {
			if cnt < *historyLength {
				latencyReport(latencies[:cnt])
			} else {
				latencyReport(latencies)
			}
			lastLatencyTime = time.Now()
		}
	}
}
