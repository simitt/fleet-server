// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package fleet

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"fleet/internal/pkg/bulk"
	"fleet/internal/pkg/dl"
	"fleet/internal/pkg/saved"

	"github.com/rs/zerolog/log"
)

const kBulkCheckinFlushInterval = 10 * time.Second

type PendingData struct {
	fields saved.Fields
	seqNo  int64
}

type BulkCheckin struct {
	bulker  bulk.Bulk
	mut     sync.Mutex
	pending map[string]PendingData
}

func NewBulkCheckin(bulker bulk.Bulk) *BulkCheckin {
	return &BulkCheckin{
		bulker:  bulker,
		pending: make(map[string]PendingData),
	}
}

func (bc *BulkCheckin) CheckIn(id string, fields saved.Fields, seqno int64) error {

	if fields == nil {
		fields = make(saved.Fields)
	}

	timeNow := time.Now().UTC().Format(time.RFC3339)
	fields[FieldLastCheckin] = timeNow

	bc.mut.Lock()
	bc.pending[id] = PendingData{fields, seqno}
	bc.mut.Unlock()
	return nil
}

func (bc *BulkCheckin) Run(ctx context.Context, sv saved.CRUD) error {

	tick := time.NewTicker(kBulkCheckinFlushInterval)

	var err error
LOOP:
	for {
		select {
		case <-tick.C:
			if err = bc.flush(ctx, sv); err != nil {
				log.Error().Err(err).Msg("Eat bulk checkin error; Keep on truckin'")
				err = nil
			}

		case <-ctx.Done():
			err = ctx.Err()
			break LOOP
		}
	}

	return err
}

func (bc *BulkCheckin) flush(ctx context.Context, sv saved.CRUD) error {
	start := time.Now()

	bc.mut.Lock()
	pending := bc.pending
	bc.pending = make(map[string]PendingData, len(pending))
	bc.mut.Unlock()

	if len(pending) == 0 {
		return nil
	}

	updates := make([]saved.UpdateT, 0, len(pending))
	seqNoUpdates := make([]bulk.BulkOp, 0, len(pending))

	for id, pendingData := range pending {
		updates = append(updates, saved.UpdateT{
			Id:     id,
			Type:   AGENT_SAVED_OBJECT_TYPE,
			Fields: pendingData.fields,
		})

		if pendingData.seqNo >= 0 {
			source, err := json.Marshal(map[string]interface{}{
				"doc": map[string]interface{}{
					"action_seq_no": pendingData.seqNo,
					"updated_at":    time.Now().UTC().Format(time.RFC3339),
				},
			})

			if err != nil {
				return err
			}

			seqNoUpdates = append(seqNoUpdates, bulk.BulkOp{
				Id:    id,
				Body:  source,
				Index: dl.FleetAgents,
			})
		}
	}

	err := sv.MUpdate(ctx, updates)

	log.Debug().
		Err(err).
		Dur("rtt", time.Since(start)).
		Int("cnt", len(updates)).
		Msg("Flush checkin")

	if err != nil {
		return err
	}

	if len(seqNoUpdates) > 0 {
		start := time.Now()
		err := bc.bulker.MUpdate(ctx, seqNoUpdates, bulk.WithRefresh())
		log.Debug().
			Err(err).
			Dur("rtt", time.Since(start)).
			Int("cnt", len(seqNoUpdates)).
			Msg("Flush seqno updates")
	}

	return err
}
