package comparator

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/grafana/loki/pkg/canary/reader"
)

const (
	ErrOutOfOrderEntry           = "out of order entry %s was received before entries: %v\n"
	ErrEntryNotReceivedWs        = "websocket failed to receive entry %v within %f seconds\n"
	ErrEntryNotReceived          = "failed to receive entry %v within %f seconds\n"
	ErrSpotCheckEntryNotReceived = "failed to find entry %v in Loki when spot check querying %v after it was written\n"
	ErrDuplicateEntry            = "received a duplicate entry for ts %v\n"
	ErrUnexpectedEntry           = "received an unexpected entry with ts %v\n"
	DebugWebsocketMissingEntry   = "websocket missing entry: %v\n"
	DebugQueryResult             = "confirmation query result: %v\n"
)

var (
	totalEntries = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "loki_canary",
		Name:      "entries_total",
		Help:      "counts log entries written to the file",
	})
	outOfOrderEntries = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "loki_canary",
		Name:      "out_of_order_entries_total",
		Help:      "counts log entries received with a timestamp more recent than the others in the queue",
	})
	wsMissingEntries = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "loki_canary",
		Name:      "websocket_missing_entries_total",
		Help:      "counts log entries not received within the maxWait duration via the websocket connection",
	})
	missingEntries = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "loki_canary",
		Name:      "missing_entries_total",
		Help:      "counts log entries not received within the maxWait duration via both websocket and direct query",
	})
	spotCheckMissing = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "loki_canary",
		Name:      "spot_check_missing_entries_total",
		Help:      "counts log entries not received when directly queried as part of spot checking",
	})
	unexpectedEntries = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "loki_canary",
		Name:      "unexpected_entries_total",
		Help:      "counts a log entry received which was not expected (e.g. received after reported missing)",
	})
	duplicateEntries = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "loki_canary",
		Name:      "duplicate_entries_total",
		Help:      "counts a log entry received more than one time",
	})
	metricTestDeviation = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "loki_canary",
		Name:      "metric_test_deviation",
		Help:      "How many counts was the actual query result from the expected based on the canary log write rate",
	})
	responseLatency prometheus.Histogram
)

type Comparator struct {
	entMtx             sync.Mutex
	spotMtx            sync.Mutex
	metTestMtx         sync.Mutex
	w                  io.Writer
	entries            []*time.Time
	spotCheck          []*time.Time
	ackdEntries        []*time.Time
	maxWait            time.Duration
	pruneInterval      time.Duration
	spotCheckInterval  time.Duration
	spotCheckMax       time.Duration
	spotCheckRunning   bool
	metricTestInterval time.Duration
	metricTestRange    string
	metricTestRagenDur time.Duration
	metricTestRunning  bool
	writeInterval      time.Duration
	confirmAsync       bool
	sent               chan time.Time
	recv               chan time.Time
	rdr                reader.LokiReader
	quit               chan struct{}
	done               chan struct{}
}

func NewComparator(writer io.Writer,
	maxWait time.Duration,
	pruneInterval time.Duration,
	spotCheckInterval, spotCheckMax time.Duration,
	metricTestInterval time.Duration,
	metricTestRange string,
	writeInterval time.Duration,
	buckets int,
	sentChan chan time.Time,
	receivedChan chan time.Time,
	reader reader.LokiReader,
	confirmAsync bool) *Comparator {
	c := &Comparator{
		w:                  writer,
		entries:            []*time.Time{},
		spotCheck:          []*time.Time{},
		maxWait:            maxWait,
		pruneInterval:      pruneInterval,
		spotCheckInterval:  spotCheckInterval,
		spotCheckMax:       spotCheckMax,
		spotCheckRunning:   false,
		metricTestInterval: metricTestInterval,
		metricTestRange:    metricTestRange,
		metricTestRunning:  false,
		writeInterval:      writeInterval,
		confirmAsync:       confirmAsync,
		sent:               sentChan,
		recv:               receivedChan,
		rdr:                reader,
		quit:               make(chan struct{}),
		done:               make(chan struct{}),
	}

	if responseLatency == nil {
		responseLatency = promauto.NewHistogram(prometheus.HistogramOpts{
			Namespace: "loki_canary",
			Name:      "response_latency",
			Help:      "is how long it takes for log lines to be returned from Loki in seconds.",
			Buckets:   prometheus.ExponentialBuckets(0.5, 2, buckets),
		})
	}
	// Convert the string to a duration to save from doing this on every query.
	// main.go should also be testing this and failing the application if this parse fails but we still need to be defensive here
	d, err := time.ParseDuration(metricTestRange)
	if err != nil {
		fmt.Fprintf(writer, "failed to parse duration string for metric-test-range, metric queries will not be possible: %v", err.Error())
		c.metricTestRagenDur = 0
	}
	c.metricTestRagenDur = d

	go c.run()

	return c
}

func (c *Comparator) Stop() {
	if c.quit != nil {
		close(c.quit)
		<-c.done
		c.quit = nil
	}
}

func (c *Comparator) entrySent(time time.Time) {
	c.entMtx.Lock()
	defer c.entMtx.Unlock()
	c.entries = append(c.entries, &time)
	//If this entry equals or exceeds the spot check interval from the last entry in the spot check array, add it.
	if len(c.spotCheck) == 0 || time.Sub(*c.spotCheck[len(c.spotCheck)-1]) >= c.spotCheckInterval {
		c.spotMtx.Lock()
		c.spotCheck = append(c.spotCheck, &time)
		c.spotMtx.Unlock()
	}
	totalEntries.Inc()
}

// entryReceived removes the received entry from the buffer if it exists, reports on out of order entries received
func (c *Comparator) entryReceived(ts time.Time) {
	c.entMtx.Lock()
	defer c.entMtx.Unlock()

	// Output index
	k := 0
	matched := false
	for i, e := range c.entries {
		if ts.Equal(*e) {
			matched = true
			// If this isn't the first item in the list we received it out of order
			if i != 0 {
				outOfOrderEntries.Inc()
				fmt.Fprintf(c.w, ErrOutOfOrderEntry, e, c.entries[:i])
			}
			responseLatency.Observe(time.Since(ts).Seconds())
			// Put this element in the acknowledged entries list so we can use it to check for duplicates
			c.ackdEntries = append(c.ackdEntries, c.entries[i])
			// Do not increment output index, effectively causing this element to be dropped
		} else {
			// If the current index doesn't match the output index, update the array with the correct position
			if i != k {
				c.entries[k] = c.entries[i]
			}
			k++
		}
	}
	if !matched {
		duplicate := false
		for _, e := range c.ackdEntries {
			if ts.Equal(*e) {
				duplicate = true
				duplicateEntries.Inc()
				fmt.Fprintf(c.w, ErrDuplicateEntry, ts.UnixNano())
				break
			}
		}
		if !duplicate {
			fmt.Fprintf(c.w, ErrUnexpectedEntry, ts.UnixNano())
			unexpectedEntries.Inc()
		}
	}
	// Nil out the pointers to any trailing elements which were removed from the slice
	for i := k; i < len(c.entries); i++ {
		c.entries[i] = nil // or the zero value of T
	}
	c.entries = c.entries[:k]
}

func (c *Comparator) Size() int {
	c.entMtx.Lock()
	defer c.entMtx.Unlock()
	return len(c.entries)
}

func (c *Comparator) run() {
	t := time.NewTicker(c.pruneInterval)
	mt := time.NewTicker(c.metricTestInterval)
	defer func() {
		t.Stop()
		mt.Stop()
		close(c.done)
	}()

	for {
		select {
		case e := <-c.recv:
			c.entryReceived(e)
		case e := <-c.sent:
			c.entrySent(e)
		case <-t.C:
			c.pruneEntries()

			// Only run one instance of spot check at a time.
			c.spotMtx.Lock()
			if !c.spotCheckRunning {
				c.spotCheckRunning = true
				go c.spotCheckEntries(time.Now())
			}
			c.spotMtx.Unlock()
		case <-mt.C:
			// Only run one intstance of metric tests at a time.
			c.metTestMtx.Lock()
			if !c.metricTestRunning {
				c.metricTestRunning = true
				go c.metricTest()
			}
			c.metTestMtx.Unlock()
		case <-c.quit:
			return
		}
	}
}

func (c *Comparator) metricTest() {
	// Always make sure to set the running state back to false
	defer func() {
		c.metTestMtx.Lock()
		c.metricTestRunning = false
		c.metTestMtx.Unlock()
	}()
	// This should never happen, main.go should fail if we failed to parse this parameter as a duration
	// but we still need to be defensive.
	if c.metricTestRagenDur == 0 {
		fmt.Fprintf(c.w, "Invalid metric-test-range parameter, metric test query failed")
		return
	}
	actualCount, err := c.rdr.QueryCountOverTime(c.metricTestRange)
	if err != nil {
		fmt.Fprintf(c.w, "error running metric query test: %s", err.Error())
	}
	expectedCount := float64(c.metricTestRagenDur.Milliseconds()) / float64(c.writeInterval.Milliseconds())
	metricTestDeviation.Set(expectedCount - actualCount)
}

func (c *Comparator) spotCheckEntries(currTime time.Time) {
	// Always make sure to set the running state back to false
	defer func() {
		c.spotMtx.Lock()
		c.spotCheckRunning = false
		c.spotMtx.Unlock()
	}()
	c.spotMtx.Lock()
	k := 0
	for i, e := range c.spotCheck {
		if e.Before(currTime.Add(-c.spotCheckMax)) {
			// Do nothing, if we don't increment the output index k, this will be dropped
		} else {
			if i != k {
				c.spotCheck[k] = c.spotCheck[i]
			}
			k++
		}
	}
	// Nil out the pointers to any trailing elements which were removed from the slice
	for i := k; i < len(c.spotCheck); i++ {
		c.spotCheck[i] = nil // or the zero value of T
	}
	c.spotCheck = c.spotCheck[:k]
	cpy := make([]*time.Time, len(c.spotCheck))
	//Make a copy so we don't have to hold the lock to verify entries
	copy(cpy, c.spotCheck)
	c.spotMtx.Unlock()

	for _, sce := range cpy {
		// Because we are querying loki timestamps vs the timestamp in the log,
		// make the range +/- 10 seconds to allow for clock inaccuracies
		start := *sce
		start = start.Add(-10 * time.Second)
		end := start.Add(10 * time.Second)
		recvd, err := c.rdr.Query(start, end)
		if err != nil {
			fmt.Fprintf(c.w, "error querying loki: %s\n", err)
			return
		}

		found := false
		for _, r := range recvd {
			if (*sce).Equal(r) {
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintf(c.w, ErrSpotCheckEntryNotReceived, sce.UnixNano(), currTime.Sub(*sce))
			spotCheckMissing.Inc()
		}
	}

}

func (c *Comparator) pruneEntries() {
	c.entMtx.Lock()
	defer c.entMtx.Unlock()

	missing := []*time.Time{}
	k := 0
	for i, e := range c.entries {
		// If the time is outside our range, assume the entry has been lost report and remove it
		if e.Before(time.Now().Add(-c.maxWait)) {
			missing = append(missing, e)
			wsMissingEntries.Inc()
			fmt.Fprintf(c.w, ErrEntryNotReceivedWs, e.UnixNano(), c.maxWait.Seconds())
		} else {
			if i != k {
				c.entries[k] = c.entries[i]
			}
			k++
		}
	}
	// Nil out the pointers to any trailing elements which were removed from the slice
	for i := k; i < len(c.entries); i++ {
		c.entries[i] = nil // or the zero value of T
	}
	c.entries = c.entries[:k]
	if len(missing) > 0 {
		if c.confirmAsync {
			go c.confirmMissing(missing)
		} else {
			c.confirmMissing(missing)
		}

	}

	// Prune the acknowledged list, remove anything older than our maxwait
	k = 0
	for i, e := range c.ackdEntries {
		if e.Before(time.Now().Add(-c.maxWait)) {
			// Do nothing, if we don't increment the output index k, this will be dropped
		} else {
			if i != k {
				c.ackdEntries[k] = c.ackdEntries[i]
			}
			k++
		}
	}
	// Nil out the pointers to any trailing elements which were removed from the slice
	for i := k; i < len(c.ackdEntries); i++ {
		c.ackdEntries[i] = nil // or the zero value of T
	}
	c.ackdEntries = c.ackdEntries[:k]
}

func (c *Comparator) confirmMissing(missing []*time.Time) {
	// Because we are querying loki timestamps vs the timestamp in the log,
	// make the range +/- 10 seconds to allow for clock inaccuracies
	start := *missing[0]
	start = start.Add(-10 * time.Second)
	end := *missing[len(missing)-1]
	end = end.Add(10 * time.Second)
	recvd, err := c.rdr.Query(start, end)
	if err != nil {
		fmt.Fprintf(c.w, "error querying loki: %s\n", err)
		return
	}
	// This is to help debug some missing log entries when queried,
	// let's print exactly what we are missing and what Loki sent back
	for _, r := range missing {
		fmt.Fprintf(c.w, DebugWebsocketMissingEntry, r.UnixNano())
	}
	for _, r := range recvd {
		fmt.Fprintf(c.w, DebugQueryResult, r.UnixNano())
	}

	k := 0
	for i, m := range missing {
		found := false
		for _, r := range recvd {
			if (*m).Equal(r) {
				// Entry was found in loki, this can be dropped from the list of missing
				// which is done by NOT incrementing the output index k
				found = true
			}
		}
		if !found {
			// Item is still missing
			if i != k {
				missing[k] = missing[i]
			}
			k++
		}
	}
	// Nil out the pointers to any trailing elements which were removed from the slice
	for i := k; i < len(missing); i++ {
		missing[i] = nil // or the zero value of T
	}
	missing = missing[:k]
	for _, e := range missing {
		missingEntries.Inc()
		fmt.Fprintf(c.w, ErrEntryNotReceived, e.UnixNano(), c.maxWait.Seconds())
	}
}
