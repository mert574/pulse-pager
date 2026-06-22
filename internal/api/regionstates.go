package api

import (
	"context"
	"strconv"

	"pulse/internal/apigen"
	"pulse/internal/checkstate"
)

// GetMonitorRegionStates returns the live per-region check state for the org's
// monitors (RFC-004 §9): each monitor maps to its regions' current state
// (scheduled/running/done/failed) with the last outcome. The frontend polls this to
// render a chip per region, page-wide. With ?monitor_id set it returns just that
// monitor. Monitors with no live state (never checked since the last restart, or whose
// state TTL lapsed) are omitted. Any member may view (it is a read, like ListMonitors).
func (s *Server) GetMonitorRegionStates(ctx context.Context, req apigen.GetMonitorRegionStatesRequestObject) (apigen.GetMonitorRegionStatesResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.GetMonitorRegionStates401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := s.canMonitor(p, authzView); !d.Allowed {
		return apigen.GetMonitorRegionStates403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}

	out := apigen.MonitorRegionStates{Monitors: map[string][]apigen.RegionState{}}
	if s.state == nil {
		return apigen.GetMonitorRegionStates200JSONResponse(out), nil
	}

	// Which monitors to read: one (the monitor_id filter, validated to be in the org) or
	// all of the org's monitors.
	var ids []int64
	if req.Params.MonitorId != nil && *req.Params.MonitorId != "" {
		id, ok := parseMonitorID(*req.Params.MonitorId)
		if !ok {
			return apigen.GetMonitorRegionStates200JSONResponse(out), nil
		}
		// Confirm the monitor is in the org (RLS-scoped) so a caller cannot probe live
		// state for another org's monitor id.
		if _, err := s.store.GetMonitor(ctx, p.OrgID, id); err != nil {
			return apigen.GetMonitorRegionStates200JSONResponse(out), nil
		}
		ids = []int64{id}
	} else {
		rows, err := s.store.ListMonitors(ctx, p.OrgID)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			ids = append(ids, row.Monitor.ID)
		}
	}
	if len(ids) == 0 {
		return apigen.GetMonitorRegionStates200JSONResponse(out), nil
	}

	states, err := checkstate.GetMany(ctx, s.state, ids)
	if err != nil {
		return nil, err
	}
	for id, regions := range states {
		out.Monitors[strconv.FormatInt(id, 10)] = regionStatesDTO(regions)
	}
	return apigen.GetMonitorRegionStates200JSONResponse(out), nil
}

// regionStatesDTO maps the stored per-region states to the API shape.
func regionStatesDTO(regions map[string]checkstate.RegionState) []apigen.RegionState {
	out := make([]apigen.RegionState, 0, len(regions))
	for region, rs := range regions {
		var reason *apigen.FailureReason
		if rs.FailureReason != nil {
			fr := apigen.FailureReason(*rs.FailureReason)
			reason = &fr
		}
		out = append(out, apigen.RegionState{
			Region:        region,
			State:         apigen.RegionStateState(rs.State),
			Healthy:       rs.Healthy,
			StatusCode:    rs.StatusCode,
			LatencyMs:     rs.LatencyMs,
			FailureReason: reason,
			UpdatedAt:     rs.UpdatedAt,
		})
	}
	return out
}
