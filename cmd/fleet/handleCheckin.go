// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"fleet/internal/pkg/action"
	"fleet/internal/pkg/bulk"
	"fleet/internal/pkg/config"
	"fleet/internal/pkg/dl"
	"fleet/internal/pkg/dsl"
	"fleet/internal/pkg/saved"

	"github.com/julienschmidt/httprouter"
	"github.com/rs/zerolog/log"
)

var (
	ErrAgentNotFound  = errors.New("agent not found")
	ErrAgentCorrupted = errors.New("agent record corrupted")

	kCheckinTimeout  = 30 * time.Second
	kLongPollTimeout = 300 * time.Second // 5m
)

func (rt Router) handleCheckin(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	// TODO: Consider rate limit here

	id := ps.ByName("id")
	err := rt.ct._handleCheckin(w, r, id, rt.sv)

	if err != nil {
		code := http.StatusBadRequest
		if err == ErrAgentNotFound {
			code = http.StatusNotFound
		}

		// Don't log connection drops
		if err != context.Canceled {
			log.Error().Err(err).Str("id", id).Int("code", code).Msg("Fail checkin")
		}
		http.Error(w, err.Error(), code)
	}
}

type CheckinT struct {
	cfg          *config.Config
	bc           *BulkCheckin
	ba           *BulkActions
	pm           *PolicyMon
	gcp          action.GlobalCheckpointProvider
	ad           *action.Dispatcher
	tr           *action.TokenResolver
	bulker       bulk.Bulk
	queryActions *dsl.Tmpl
}

func NewCheckinT(
	cfg *config.Config,
	bc *BulkCheckin,
	ba *BulkActions,
	pm *PolicyMon,
	gcp action.GlobalCheckpointProvider,
	ad *action.Dispatcher,
	tr *action.TokenResolver,
	bulker bulk.Bulk,
) *CheckinT {
	queryActions, err := dl.PrepareAgentActionsQuery()
	if err != nil {
		panic(err)
	}
	return &CheckinT{
		cfg:          cfg,
		bc:           bc,
		ba:           ba,
		pm:           pm,
		gcp:          gcp,
		ad:           ad,
		tr:           tr,
		bulker:       bulker,
		queryActions: queryActions,
	}
}

func (ct *CheckinT) _handleCheckin(w http.ResponseWriter, r *http.Request, id string, sv saved.CRUD) error {

	agent, err := authAgent(r, id, sv, ct.bulker)

	if err != nil {
		return err
	}

	ctx := r.Context()

	// Interpret request; TODO: defend overflow, slow roll
	var req CheckinRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&req); err != nil {
		return err
	}

	// Compare local_metadata content and update if different
	fields, err := parseMeta(agent, &req)
	if err != nil {
		return err
	}

	actionSub, err := ct.ba.Subscribe(agent.Id)
	if err != nil {
		return err
	}
	defer ct.ba.Unsubscribe(actionSub)
	actionCh := actionSub.Ch()

	var actCh chan []dl.ActionDoc
	var seqno int64
	if ct.cfg.Features.Enabled(config.FeatureActions) {
		// Resolve AckToken from request, fallback on the agent record
		seqno, err := ct.resolveSeqNo(ctx, req, agent)
		if err != nil {
			return err
		}

		// Subsribe to actions dispatcher
		aSub := ct.ad.Subscribe(agent.Id, seqno)
		defer ct.ad.Unsubscribe(aSub)
		actCh = aSub.Ch()
	}

	// Subscribe to policy manager for changes on PolicyId > policyRev
	sub, err := ct.pm.Subscribe(agent.PolicyId, agent.PolicyRev)
	if err != nil {
		return err
	}
	defer ct.pm.Unsubscribe(*sub)

	// Update check-in timestamp on timeout
	tick := time.NewTicker(kCheckinTimeout)
	defer tick.Stop()

	// Chill out for for a bit. Long poll.
	longPoll := time.NewTicker(kLongPollTimeout)
	defer longPoll.Stop()

	// Intial update on checkin, and any user fields that might have changed
	ct.bc.CheckIn(agent.Id, fields, seqno)

	// Initial fetch for pending actions
	var (
		actions  []ActionResp
		ackToken string
	)

	if ct.cfg.Features.Enabled(config.FeatureActions) {
		pendingActions, err := ct.fetchAgentPendingActions(ctx, seqno, agent.Id)
		if err != nil {
			return err
		}
		actions, ackToken = convertActionsX(agent.Id, pendingActions)
	}

	if len(actions) == 0 {
	LOOP:
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case acdocs := <-actCh:
				var acs []ActionResp
				acs, ackToken = convertActionsX(agent.Id, acdocs)
				actions = append(actions, acs...)
				break LOOP
			case actionList := <-actionCh:
				actions = append(actions, convertActions(actionList)...)
				break LOOP
			case action := <-sub.C:
				actionResp, err := parsePolicy(ctx, sv, agent.Id, action)
				if err != nil {
					return err
				}
				actions = append(actions, *actionResp)
				break LOOP
			case <-longPoll.C:
				log.Trace().Msg("Fire long poll")
				break LOOP
			case <-tick.C:
				ct.bc.CheckIn(agent.Id, nil, seqno)
			}
		}
	}

	// For now, empty response
	resp := CheckinResponse{
		AckToken: ackToken,
		Action:   "checkin",
		Actions:  actions,
	}

	data, err := json.Marshal(&resp)
	if err != nil {
		return err
	}

	if _, err = w.Write(data); err != nil {
		return err
	}

	log.Trace().RawJSON("resp", data).Msg("Checkin response")

	return nil
}

// Resolve AckToken from request, fallback on the agent record
func (ct *CheckinT) resolveSeqNo(ctx context.Context, req CheckinRequest, agent *Agent) (seqno int64, err error) {
	// Resolve AckToken from request, fallback on the agent record
	ackToken := req.AckToken
	seqno = agent.ActionSeqNo

	if ct.tr != nil && ackToken != "" {
		var sn int64
		sn, err = ct.tr.Resolve(ctx, ackToken)
		if err != nil {
			if errors.Is(err, dl.ErrNotFound) {
				log.Debug().Str("token", ackToken).Str("agent_id", agent.Id).Msg("Token not found")
				err = nil
			} else {
				return
			}
		}
		seqno = sn
	}
	return seqno, nil
}

func (ct *CheckinT) fetchAgentPendingActions(ctx context.Context, seqno int64, agentId string) ([]dl.ActionDoc, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	return dl.SearchActions(ctx, ct.bulker, ct.queryActions, map[string]interface{}{
		dl.FieldSeqNo:      seqno,
		dl.FieldMaxSeqNo:   ct.gcp.GetCheckpoint(),
		dl.FieldExpiration: now,
		dl.FieldAgents:     []string{agentId},
	})
}

func convertActions(actionList []Action) []ActionResp {
	respList := make([]ActionResp, 0, len(actionList))
	for _, action := range actionList {
		respList = append(respList, ActionResp{
			AgentId:   action.AgentId,
			CreatedAt: action.CreatedAt,
			Data:      []byte(action.Data),
			Id:        action.Id,
			Type:      action.Type,
		})
	}

	return respList
}

func convertActionsX(agentId string, acdocs []dl.ActionDoc) ([]ActionResp, string) {
	var ackToken string
	sz := len(acdocs)

	respList := make([]ActionResp, 0, sz)
	for _, acdoc := range acdocs {
		respList = append(respList, ActionResp{
			AgentId:   agentId,
			CreatedAt: acdoc.Timestamp,
			Data:      []byte(acdoc.Data),
			Id:        acdoc.Id,
			Type:      acdoc.Type,
		})
	}

	if sz > 0 {
		ackToken = acdocs[sz-1].DocID
	}

	return respList, ackToken
}

func parsePolicy(ctx context.Context, sv saved.CRUD, agentId string, action Action) (*ActionResp, error) {
	// Need to inject the default api key into the object. So:
	// 1) Deserialize the action
	// 2) Lookup the DefaultApiKey in the save agent (we purposefully didn't decode it before)
	// 3) If not there, generate and persist DefaultAPIKey
	// 4) Inject default api key into structure
	// 5) Reserialize and return AgentResp structre

	var actionObj map[string]interface{}
	if err := json.Unmarshal([]byte(action.Data), &actionObj); err != nil {
		return nil, err
	}

	// Repull and decode the agent object
	var agent Agent
	if err := sv.Read(ctx, AGENT_SAVED_OBJECT_TYPE, agentId, &agent); err != nil {
		return nil, err
	}

	if agent.DefaultApiKey == "" {
		defaultOutputApiKey, err := generateOutputApiKey(ctx, sv.Client(), agent.Id, "default")
		if err != nil {
			return nil, err
		}
		agent.DefaultApiKey = defaultOutputApiKey.Token()
		agent.DefaultApiKeyId = defaultOutputApiKey.Id

		// TODO: Consider how to fix update to do this?
		opts := []saved.Option{
			saved.WithId(agentId),
			saved.WithOverwrite(),
			saved.WithRefresh(),
		}

		log.Info().Str("agentId", agentId).Msg("Rewriting full agent record to pick up deafult output key.")
		if _, err = sv.Create(ctx, AGENT_SAVED_OBJECT_TYPE, agent, opts...); err != nil {
			return nil, err
		}
	}

	var ok bool
	if ok = setMapObj(actionObj, agent.DefaultApiKey, "policy", "outputs", "default", "api_key"); !ok {
		ok = setMapObj(actionObj, agent.DefaultApiKey, "policy", "config", "default", "api_key")
	}

	dataJSON := []byte(action.Data)
	if ok {
		// Reserialize
		var err error
		if dataJSON, err = json.Marshal(actionObj); err != nil {
			return nil, err
		}
	} else {
		log.Debug().Msg("Cannot inject api_key into action")
	}

	resp := ActionResp{
		AgentId:   agent.Id,
		CreatedAt: action.CreatedAt,
		Data:      dataJSON,
		Id:        action.Id,
		Type:      action.Type,
	}

	return &resp, nil
}

func setMapObj(obj map[string]interface{}, val interface{}, keys ...string) bool {
	if len(keys) == 0 {
		return false
	}

	for _, k := range keys[:len(keys)-1] {
		v, ok := obj[k]
		if !ok {
			return false
		}

		obj, ok = v.(map[string]interface{})
		if !ok {
			return false
		}
	}

	k := keys[len(keys)-1]
	obj[k] = val

	return true
}

// Node.JS Fleet does this on a shared timer; not sure why is more efficient.
func updateCheckinTimestamp(ctx context.Context, sv saved.CRUD, agent *Agent, fields saved.Fields) error {
	timeNow := time.Now().UTC().Format(time.RFC3339)

	if fields == nil {
		fields = make(saved.Fields)
	}

	fields[FieldLastCheckin] = timeNow

	return sv.Update(ctx, AGENT_SAVED_OBJECT_TYPE, agent.Id, fields)
}

func findAgentByApiKeyId(ctx context.Context, sv saved.CRUD, id string) (*Agent, error) {

	raw, err := apiKeyQueryTmpl.RenderOne(kTmplApiKeyField, id)
	if err != nil {
		return nil, err
	}

	// Pull API key record from saved objects
	hits, err := sv.FindRaw(ctx, raw)
	if err != nil {
		return nil, err
	}

	if len(hits) == 0 {
		return nil, ErrAgentNotFound
	}

	// Expect only one hit
	if len(hits) != 1 {
		return nil, fmt.Errorf("hit count mismatch %v", len(hits))
	}

	// Don't bother decrypting agent key; do straight decode instead of saved.Decode
	var agent Agent
	if err := json.Unmarshal(hits[0].Data, &agent); err != nil {
		return nil, err
	}

	agent.Id = hits[0].Id
	return &agent, nil
}

// parseMeta compares the agent and the request local_metadata content
// and returns fields to update the agent record or nil
func parseMeta(agent *Agent, req *CheckinRequest) (fields saved.Fields, err error) {
	// Quick comparison first
	if bytes.Equal(req.LocalMeta, agent.LocalMeta) {
		log.Trace().Msg("Quick comparing local metadata is equal")
		return nil, nil
	}

	// Compare local_metadata content and update if different
	var reqLocalMeta saved.Fields
	var agentLocalMeta saved.Fields
	err = json.Unmarshal(req.LocalMeta, &reqLocalMeta)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(agent.LocalMeta, &agentLocalMeta)
	if err != nil {
		return nil, err
	}

	if reqLocalMeta != nil && !reflect.DeepEqual(reqLocalMeta, agentLocalMeta) {
		log.Info().RawJSON("req.LocalMeta", req.LocalMeta).Msg("Applying new local metadata")
		fields = map[string]interface{}{
			FieldLocalMetadata: req.LocalMeta,
		}
	}
	return fields, nil
}
