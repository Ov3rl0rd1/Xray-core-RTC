// Package command exposes the fork's policy and usage store over gRPC, so an
// external panel can drive plans, quotas and device limits and be told about
// changes as they happen.
//
// It is registered like any other Xray API service — add "TariffService" to
// api.services — and therefore shares the commander's listener, its routing
// protection and its lifecycle. A panel talks to one endpoint for
// HandlerService, StatsService and this.
package command

import (
	"context"

	"github.com/xtls/xray-core/app/tariff"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/errors"
	"google.golang.org/grpc"
)

// service implements TariffServiceServer against a [tariff.Manager].
type service struct {
	UnimplementedTariffServiceServer

	manager *tariff.Manager
}

// NewService returns a gRPC service backed by the given manager.
func NewService(m *tariff.Manager) TariffServiceServer {
	return &service{manager: m}
}

func (s *service) SetPolicy(_ context.Context, req *SetPolicyRequest) (*SetPolicyResponse, error) {
	if err := s.manager.SetPolicy(req.GetPolicy()); err != nil {
		return nil, err
	}
	return &SetPolicyResponse{}, nil
}

// SetPolicies applies a roster in one call. With replace set, users the server
// holds but the roster does not name are dropped — so a panel can converge the
// server on its own view without having to compute a diff, and a subscription
// cancelled while this server was unreachable does not outlive the next sync.
func (s *service) SetPolicies(_ context.Context, req *SetPoliciesRequest) (*SetPoliciesResponse, error) {
	resp := &SetPoliciesResponse{}
	keep := make(map[string]bool, len(req.GetPolicies()))
	for _, p := range req.GetPolicies() {
		if err := s.manager.SetPolicy(p); err != nil {
			return nil, err
		}
		keep[p.GetEmail()] = true
		resp.Applied++
	}
	if req.GetReplace() {
		for _, p := range s.manager.Policies() {
			if keep[p.GetEmail()] {
				continue
			}
			if err := s.manager.RemovePolicy(p.GetEmail()); err == nil {
				resp.Removed++
			}
		}
	}
	return resp, nil
}

func (s *service) GetPolicy(_ context.Context, req *GetPolicyRequest) (*GetPolicyResponse, error) {
	p := s.manager.Policy(req.GetEmail())
	if p == nil {
		return nil, errors.New("tariff: no policy for ", req.GetEmail())
	}
	return &GetPolicyResponse{Policy: p}, nil
}

func (s *service) RemovePolicy(_ context.Context, req *RemovePolicyRequest) (*RemovePolicyResponse, error) {
	if err := s.manager.RemovePolicy(req.GetEmail()); err != nil {
		return nil, err
	}
	return &RemovePolicyResponse{}, nil
}

func (s *service) ListPolicies(context.Context, *ListPoliciesRequest) (*ListPoliciesResponse, error) {
	return &ListPoliciesResponse{Policies: s.manager.Policies()}, nil
}

func (s *service) GetUsage(_ context.Context, req *GetUsageRequest) (*GetUsageResponse, error) {
	if email := req.GetEmail(); email != "" {
		u := s.manager.Usage(email)
		if u == nil {
			return nil, errors.New("tariff: no usage for ", email)
		}
		return &GetUsageResponse{Users: []*tariff.Usage{u}}, nil
	}
	return &GetUsageResponse{Users: s.manager.AllUsage()}, nil
}

func (s *service) AddUsage(_ context.Context, req *AddUsageRequest) (*AddUsageResponse, error) {
	err := s.manager.AddUsage(req.GetEmail(), req.GetInboundTag(), req.GetUplink(), req.GetDownlink())
	if err != nil {
		return nil, err
	}
	return &AddUsageResponse{}, nil
}

func (s *service) ResetUsage(_ context.Context, req *ResetUsageRequest) (*ResetUsageResponse, error) {
	if req.GetEmail() == "" {
		s.manager.ResetServerUsage()
		return &ResetUsageResponse{}, nil
	}
	if err := s.manager.ResetUsage(req.GetEmail()); err != nil {
		return nil, err
	}
	return &ResetUsageResponse{}, nil
}

func (s *service) SetServerQuota(_ context.Context, req *SetServerQuotaRequest) (*SetServerQuotaResponse, error) {
	s.manager.SetServerQuota(req.GetQuota())
	return &SetServerQuotaResponse{}, nil
}

func (s *service) SetConfig(_ context.Context, req *SetConfigRequest) (*SetConfigResponse, error) {
	s.manager.SetConfig(req.GetConfig())
	return &SetConfigResponse{}, nil
}

func (s *service) GetStatus(context.Context, *GetStatusRequest) (*GetStatusResponse, error) {
	users := s.manager.AllUsage()
	resp := &GetStatusResponse{
		Users:            uint32(len(users)),
		EventSubscribers: uint32(s.manager.Subscribers()),
		ServerQuota:      s.manager.ServerUsage(),
		Config:           s.manager.Config(),
	}
	for _, u := range users {
		resp.Connections += u.GetConnections()
		resp.Devices += uint32(len(u.GetDevices()))
	}
	return resp, nil
}

func (s *service) Flush(context.Context, *FlushRequest) (*FlushResponse, error) {
	if err := s.manager.Flush(); err != nil {
		return nil, err
	}
	return &FlushResponse{}, nil
}

// StreamEvents delivers changes until the client goes away.
//
// The manager never blocks on a subscriber, so a stream that stops being read
// loses events instead of stalling the data plane. Each message carries how
// many this stream has missed since it started, which is the signal for a
// client to reconcile with GetUsage rather than assume it has seen everything.
func (s *service) StreamEvents(req *StreamEventsRequest, stream grpc.ServerStreamingServer[StreamEventsResponse]) error {
	id, events, dropped := s.manager.Subscribe()
	defer s.manager.Unsubscribe(id)

	kinds := make(map[tariff.EventKind]bool, len(req.GetKinds()))
	for _, k := range req.GetKinds() {
		kinds[k] = true
	}
	email := req.GetEmail()

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case e, ok := <-events:
			if !ok {
				return nil
			}
			if len(kinds) > 0 && !kinds[e.GetKind()] {
				continue
			}
			if email != "" && e.GetEmail() != email {
				continue
			}
			err := stream.Send(&StreamEventsResponse{Event: e, Dropped: dropped.Load()})
			if err != nil {
				return err
			}
		}
	}
}

// Register implements commander.Service.
func (s *service) Register(server *grpc.Server) {
	RegisterTariffServiceServer(server, s)
}

func init() {
	common.Must(common.RegisterConfig((*Config)(nil), func(_ context.Context, _ interface{}) (interface{}, error) {
		return &service{manager: tariff.Default()}, nil
	}))
}
