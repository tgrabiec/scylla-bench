package main

import (
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codahale/hdrhistogram"
	"github.com/gocql/gocql"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"
)

type RateLimiter interface {
	Wait()
	ExpectedInterval() int64
}

type UnlimitedRateLimiter struct{}

func (*UnlimitedRateLimiter) Wait() {}

func (*UnlimitedRateLimiter) ExpectedInterval() int64 {
	return 0
}

type MaximumRateLimiter struct {
	Period              time.Duration
	StartTime           time.Time
	CompletedOperations int64
}

func (mxrl *MaximumRateLimiter) Wait() {
	mxrl.CompletedOperations++
	nextRequest := mxrl.StartTime.Add(mxrl.Period * time.Duration(mxrl.CompletedOperations))
	now := time.Now()
	if now.Before(nextRequest) {
		time.Sleep(nextRequest.Sub(now))
	}
}

func (mxrl *MaximumRateLimiter) ExpectedInterval() int64 {
	return mxrl.Period.Nanoseconds()
}

func NewRateLimiter(maximumRate int, timeOffset time.Duration) RateLimiter {
	if maximumRate == 0 {
		return &UnlimitedRateLimiter{}
	}
	period := time.Duration(int64(time.Second) / int64(maximumRate))
	return &MaximumRateLimiter{period, time.Now(), 0}
}

type Result struct {
	Final          bool
	ElapsedTime    time.Duration
	Operations     int
	ClusteringRows int
	Errors         int
	Latency        *hdrhistogram.Histogram
}

type MergedResult struct {
	Time                    time.Duration
	Operations              int
	ClusteringRows          int
	OperationsPerSecond     float64
	ClusteringRowsPerSecond float64
	Errors                  int
	Latency                 *hdrhistogram.Histogram
}

func NewMergedResult() *MergedResult {
	result := &MergedResult{}
	result.Latency = NewHistogram()
	return result
}

func (mr *MergedResult) AddResult(result Result) {
	mr.Time += result.ElapsedTime
	mr.Operations += result.Operations
	mr.ClusteringRows += result.ClusteringRows
	mr.OperationsPerSecond += float64(result.Operations) / result.ElapsedTime.Seconds()
	mr.ClusteringRowsPerSecond += float64(result.ClusteringRows) / result.ElapsedTime.Seconds()
	mr.Errors += result.Errors
	if measureLatency {
		dropped := mr.Latency.Merge(result.Latency)
		if dropped > 0 {
			log.Print("dropped: ", dropped)
		}
	}
}

func NewHistogram() *hdrhistogram.Histogram {
	if !measureLatency {
		return nil
	}
	return hdrhistogram.New(time.Microsecond.Nanoseconds()*50, (timeout + timeout*2).Nanoseconds(), 3)
}

func HandleError(err error) {
	if atomic.SwapUint32(&stopAll, 1) == 0 {
		log.Print(err)
		fmt.Println("\nstopping")
		atomic.StoreUint32(&stopAll, 1)
	}
}

func MergeResults(results []chan Result) (bool, *MergedResult) {
	result := NewMergedResult()
	final := false
	for i, ch := range results {
		res := <-ch
		if !final && res.Final {
			final = true
			result = NewMergedResult()
			for _, ch2 := range results[0:i] {
				res = <-ch2
				for !res.Final {
					res = <-ch2
				}
				result.AddResult(res)
			}
		} else if final && !res.Final {
			for !res.Final {
				res = <-ch
			}
		}
		result.AddResult(res)
	}
	result.Time /= time.Duration(concurrency)
	return final, result
}

func RunConcurrently(name string, maximumRate int, workload func(id int, resultChannel chan Result, rateLimiter RateLimiter)) *MergedResult {
	var timeOffsetUnit int64
	if maximumRate != 0 {
		timeOffsetUnit = int64(time.Second) / int64(maximumRate)
		maximumRate /= concurrency
	} else {
		timeOffsetUnit = 0
	}

	results := make([]chan Result, concurrency)
	for i := range results {
		results[i] = make(chan Result, 1)
	}

	startTime := time.Now()
	for i := 0; i < concurrency; i++ {
		go func(i int) {
			timeOffset := time.Duration(timeOffsetUnit * int64(i))
			workload(i, results[i], NewRateLimiter(maximumRate, timeOffset))
			close(results[i])
		}(i)
	}

	lat_max := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lat_max",
		Help: "Highest latency",
	})

	lat_99 := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lat_99",
		Help: "99th percentile latency",
	})

	lat_90 := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lat_90",
		Help: "90th percentile latency",
	})

	lat_50 := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lat_50",
		Help: "50th percentile latency",
	})

	registry := prometheus.NewRegistry()
	registry.MustRegister(lat_max, lat_99, lat_90, lat_50)
	pusher := push.New("http://127.0.0.1:9091", name).Gatherer(registry)

	final, result := MergeResults(results)
	for !final {
		result.Time = time.Now().Sub(startTime)

		lat_max.Set(float64(result.Latency.Max()))
		lat_99.Set(float64(result.Latency.ValueAtQuantile(99)))
		lat_90.Set(float64(result.Latency.ValueAtQuantile(90)))
		lat_50.Set(float64(result.Latency.ValueAtQuantile(50)))
		pusher.Push()

		PrintPartialResult(result)
		final, result = MergeResults(results)
	}
	return result
}

type ResultBuilder struct {
	FullResult    *Result
	PartialResult *Result
}

func NewResultBuilder() *ResultBuilder {
	rb := &ResultBuilder{}
	rb.FullResult = &Result{}
	rb.PartialResult = &Result{}
	rb.FullResult.Final = true
	rb.FullResult.Latency = NewHistogram()
	rb.PartialResult.Latency = NewHistogram()
	return rb
}

func (rb *ResultBuilder) IncOps() {
	rb.FullResult.Operations++
	rb.PartialResult.Operations++
}

func (rb *ResultBuilder) IncRows() {
	rb.FullResult.ClusteringRows++
	rb.PartialResult.ClusteringRows++
}

func (rb *ResultBuilder) AddRows(n int) {
	rb.FullResult.ClusteringRows += n
	rb.PartialResult.ClusteringRows += n
}

func (rb *ResultBuilder) IncErrors() {
	rb.FullResult.Errors++
	rb.PartialResult.Errors++
}

func (rb *ResultBuilder) ResetPartialResult() {
	rb.PartialResult = &Result{}
	rb.PartialResult.Latency = NewHistogram()
}

func (rb *ResultBuilder) RecordLatency(latency time.Duration, rateLimiter RateLimiter) error {
	if !measureLatency {
		return nil
	}

	err := rb.FullResult.Latency.RecordCorrectedValue(latency.Nanoseconds(), rateLimiter.ExpectedInterval())
	if err != nil {
		return err
	}

	err = rb.PartialResult.Latency.RecordCorrectedValue(latency.Nanoseconds(), rateLimiter.ExpectedInterval())
	if err != nil {
		return err
	}

	return nil
}

var errorRecordingLatency bool

func RunTest(resultChannel chan Result, workload WorkloadGenerator, rateLimiter RateLimiter, test func(rb *ResultBuilder) (error, time.Duration)) {
	rb := NewResultBuilder()

	start := time.Now()
	partialStart := start
	for !workload.IsDone() && atomic.LoadUint32(&stopAll) == 0 {
		rateLimiter.Wait()

		err, latency := test(rb)
		if err != nil {
			log.Print(err)
			rb.IncErrors()
			continue
		}

		err = rb.RecordLatency(latency, rateLimiter)
		if err != nil {
			errorRecordingLatency = true
		}

		now := time.Now()
		if now.Sub(partialStart) > time.Second {
			resultChannel <- *rb.PartialResult
			rb.ResetPartialResult()
			partialStart = now
		}
	}
	end := time.Now()

	rb.FullResult.ElapsedTime = end.Sub(start)
	resultChannel <- *rb.FullResult
}

func DoWrites(session *gocql.Session, resultChannel chan Result, workload WorkloadGenerator, rateLimiter RateLimiter) {
	value := make([]byte, clusteringRowSize)
	query := session.Query("INSERT INTO " + keyspaceName + "." + tableName + " (pk, ck, v) VALUES (?, ?, ?)")

	RunTest(resultChannel, workload, rateLimiter, func(rb *ResultBuilder) (error, time.Duration) {
		pk := workload.NextPartitionKey()
		ck := workload.NextClusteringKey()
		bound := query.Bind(pk, ck, value)

		requestStart := time.Now()
		err := bound.Exec()
		requestEnd := time.Now()
		if err != nil {
			return err, time.Duration(0)
		}

		rb.IncOps()
		rb.IncRows()

		latency := requestEnd.Sub(requestStart)
		return nil, latency
	})
}

func DoBatchedWrites(session *gocql.Session, resultChannel chan Result, workload WorkloadGenerator, rateLimiter RateLimiter) {
	value := make([]byte, clusteringRowSize)
	request := fmt.Sprintf("INSERT INTO %s.%s (pk, ck, v) VALUES (?, ?, ?)", keyspaceName, tableName)

	RunTest(resultChannel, workload, rateLimiter, func(rb *ResultBuilder) (error, time.Duration) {
		batch := gocql.NewBatch(gocql.UnloggedBatch)
		batchSize := 0

		currentPk := workload.NextPartitionKey()
		for !workload.IsPartitionDone() && atomic.LoadUint32(&stopAll) == 0 && batchSize < rowsPerRequest {
			ck := workload.NextClusteringKey()
			batchSize++
			batch.Query(request, currentPk, ck, value)
		}

		requestStart := time.Now()
		err := session.ExecuteBatch(batch)
		requestEnd := time.Now()
		if err != nil {
			return err, time.Duration(0)
		}

		rb.IncOps()
		rb.AddRows(batchSize)

		latency := requestEnd.Sub(requestStart)
		return nil, latency
	})
}

func DoCounterUpdates(session *gocql.Session, resultChannel chan Result, workload WorkloadGenerator, rateLimiter RateLimiter) {
	query := session.Query("UPDATE " + keyspaceName + "." + counterTableName + " SET c1 = c1 + 1, c2 = c2 + 1, c3 = c3 + 1, c4 = c4 + 1, c5 = c5 + 1 WHERE pk = ? AND ck = ?")

	RunTest(resultChannel, workload, rateLimiter, func(rb *ResultBuilder) (error, time.Duration) {
		pk := workload.NextPartitionKey()
		ck := workload.NextClusteringKey()
		bound := query.Bind(pk, ck)

		requestStart := time.Now()
		err := bound.Exec()
		requestEnd := time.Now()
		if err != nil {
			return err, time.Duration(0)
		}

		rb.IncOps()
		rb.IncRows()

		latency := requestEnd.Sub(requestStart)
		return nil, latency
	})
}

func DoReads(session *gocql.Session, resultChannel chan Result, workload WorkloadGenerator, rateLimiter RateLimiter) {
	DoReadsFromTable(tableName, session, resultChannel, workload, rateLimiter)
}

func DoCounterReads(session *gocql.Session, resultChannel chan Result, workload WorkloadGenerator, rateLimiter RateLimiter) {
	DoReadsFromTable(counterTableName, session, resultChannel, workload, rateLimiter)
}

func DoReadsFromTable(table string, session *gocql.Session, resultChannel chan Result, workload WorkloadGenerator, rateLimiter RateLimiter) {
	var request string
	if inRestriction {
		arr := make([]string, rowsPerRequest)
		for i := 0; i < rowsPerRequest; i++ {
			arr[i] = "?"
		}
		request = fmt.Sprintf("SELECT * from %s.%s WHERE pk = ? AND ck IN (%s)", keyspaceName, table, strings.Join(arr, ", "))
	} else if provideUpperBound {
		request = fmt.Sprintf("SELECT * FROM %s.%s WHERE pk = ? AND ck >= ? AND ck < ?", keyspaceName, table)
	} else if noLowerBound {
		request = fmt.Sprintf("SELECT * FROM %s.%s WHERE pk = ? LIMIT %d", keyspaceName, table, rowsPerRequest)
	} else {
		request = fmt.Sprintf("SELECT * FROM %s.%s WHERE pk = ? AND ck >= ? LIMIT %d", keyspaceName, table, rowsPerRequest)
	}
	query := session.Query(request)

	RunTest(resultChannel, workload, rateLimiter, func(rb *ResultBuilder) (error, time.Duration) {
		pk := workload.NextPartitionKey()

		var bound *gocql.Query
		if inRestriction {
			args := make([]interface{}, 1, rowsPerRequest+1)
			args[0] = pk
			for i := 0; i < rowsPerRequest; i++ {
				if workload.IsPartitionDone() {
					args = append(args, 0)
				} else {
					args = append(args, workload.NextClusteringKey())
				}
			}
			bound = query.Bind(args...)
		} else if noLowerBound {
			bound = query.Bind(pk)
		} else {
			ck := workload.NextClusteringKey()
			if provideUpperBound {
				bound = query.Bind(pk, ck, ck+int64(rowsPerRequest))
			} else {
				bound = query.Bind(pk, ck)
			}
		}

		requestStart := time.Now()
		iter := bound.Iter()
		if table == tableName {
			for iter.Scan(nil, nil, nil) {
				rb.IncRows()
			}
		} else {
			for iter.Scan(nil, nil, nil, nil, nil, nil, nil) {
				rb.IncRows()
			}
		}
		requestEnd := time.Now()

		err := iter.Close()
		if err != nil {
			return err, time.Duration(0)
		}

		rb.IncOps()

		latency := requestEnd.Sub(requestStart)
		return nil, latency
	})
}
