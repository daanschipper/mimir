// SPDX-License-Identifier: AGPL-3.0-only

package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/cancellation"
	"github.com/grafana/dskit/user"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/grafana/mimir/pkg/mimirpb"
)

type Pusher interface {
	PushToStorage(context.Context, *mimirpb.WriteRequest) error
}

type pusherConsumer struct {
	p Pusher

	processingTimeSeconds prometheus.Observer
	clientErrRequests     prometheus.Counter
	serverErrRequests     prometheus.Counter
	totalRequests         prometheus.Counter
	l                     log.Logger
}

type parsedRecord struct {
	*mimirpb.WriteRequest
	tenantID string
	err      error
}

func newPusherConsumer(p Pusher, reg prometheus.Registerer, l log.Logger) *pusherConsumer {
	errRequestsCounter := promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Name: "cortex_ingest_storage_reader_records_failed_total",
		Help: "Number of records (write requests) which caused errors while processing. Client errors are errors such as tenant limits and samples out of bounds. Server errors indicate internal recoverable errors.",
	}, []string{"cause"})

	return &pusherConsumer{
		p: p,
		l: l,
		processingTimeSeconds: promauto.With(reg).NewHistogram(prometheus.HistogramOpts{
			Name:                            "cortex_ingest_storage_reader_processing_time_seconds",
			Help:                            "Time taken to process a single record (write request).",
			NativeHistogramBucketFactor:     1.1,
			NativeHistogramMaxBucketNumber:  100,
			NativeHistogramMinResetDuration: 1 * time.Hour,
			Buckets:                         prometheus.DefBuckets,
		}),
		clientErrRequests: errRequestsCounter.WithLabelValues("client"),
		serverErrRequests: errRequestsCounter.WithLabelValues("server"),
		totalRequests: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "cortex_ingest_storage_reader_records_total",
			Help: "Number of attempted records (write requests).",
		}),
	}
}

func (c pusherConsumer) consume(ctx context.Context, records []record) error {
	recC := make(chan parsedRecord)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(cancellation.NewErrorf("done consuming records"))

	// Speed up consumption by unmarhsalling the next request while the previous one is being pushed.
	go c.unmarshalRequests(ctx, records, recC)
	err := c.pushRequests(ctx, recC)
	if err != nil {
		return err
	}
	return nil
}

func (c pusherConsumer) pushRequests(ctx context.Context, reqC <-chan parsedRecord) error {
	recordIdx := -1
	for wr := range reqC {
		recordIdx++
		if wr.err != nil {
			level.Error(c.l).Log("msg", "failed to parse write request; skipping", "err", wr.err)
			continue
		}
		processingStart := time.Now()

		ctx := user.InjectOrgID(ctx, wr.tenantID)
		err := c.p.PushToStorage(ctx, wr.WriteRequest)

		processingElapsedTime := time.Since(processingStart)
		c.processingTimeSeconds.Observe(processingElapsedTime.Seconds())
		c.totalRequests.Inc()

		if err != nil {
			if !mimirpb.IsClientError(err) {
				c.serverErrRequests.Inc()
				return fmt.Errorf("consuming record at index %d for tenant %s: %w", recordIdx, wr.tenantID, err)
			}
			c.clientErrRequests.Inc()

			// The error could be sampled or marked to be skipped in logs, so we check whether it should be
			// logged before doing it.
			if shouldLog(ctx, err, processingElapsedTime) {
				level.Warn(c.l).Log("msg", "detected a client error while ingesting write request (the request may have been partially ingested)", "err", err, "user", wr.tenantID)
			}
		}
	}
	return nil
}

func (c pusherConsumer) unmarshalRequests(ctx context.Context, records []record, recC chan<- parsedRecord) {
	defer close(recC)
	done := ctx.Done()

	for _, record := range records {
		pRecord := parsedRecord{
			tenantID:     record.tenantID,
			WriteRequest: &mimirpb.WriteRequest{},
		}
		// We don't free the WriteRequest slices because they are being freed by the Pusher.
		err := pRecord.WriteRequest.Unmarshal(record.content)
		if err != nil {
			err = errors.Wrap(err, "parsing ingest consumer write request")
			pRecord.err = err
		}
		select {
		case <-done:
			return
		case recC <- pRecord:
		}
	}
}
