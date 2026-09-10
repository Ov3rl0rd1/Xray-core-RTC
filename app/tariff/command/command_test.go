package command_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/xtls/xray-core/app/tariff"
	"github.com/xtls/xray-core/app/tariff/command"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/infra/conf"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const mb = 1024 * 1024

// dial spins the service up over an in-process connection, so the tests
// exercise the generated stubs and the wire types rather than the Go methods
// underneath them.
func dial(t *testing.T) (command.TariffServiceClient, *tariff.Manager) {
	t.Helper()

	// A manager of its own, not tariff.Default(): the default installs itself
	// into the process-wide dispatcher, which tests have no business doing.
	m := tariff.New(nil)
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	command.RegisterTariffServiceServer(srv, command.NewService(m))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return command.NewTariffServiceClient(conn), m
}

func TestAPIConfigResolvesTariffService(t *testing.T) {
	cfg := &conf.APIConfig{
		Tag:      "api",
		Services: []string{"HandlerService", "StatsService", "TariffService"},
	}
	built, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}

	want := serial.ToTypedMessage(&command.Config{}).Type
	for _, s := range built.Service {
		if s.Type == want {
			return
		}
	}
	t.Fatalf("TariffService did not resolve; api.services produced %d services, none of type %q",
		len(built.Service), want)
}

func TestAPIConfigIgnoresUnknownServices(t *testing.T) {
	built, err := (&conf.APIConfig{Tag: "api", Services: []string{"NoSuchService"}}).Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(built.Service) != 0 {
		t.Fatalf("an unknown service name produced %d services, want 0", len(built.Service))
	}
}

func TestPolicyRoundTripsOverTheWire(t *testing.T) {
	client, _ := dial(t)
	ctx := context.Background()

	policy := &tariff.Policy{
		Email:       "alice@example",
		UplinkBps:   10 * mb,
		DownlinkBps: 20 * mb,
		MaxDevices:  3,
		Quotas: []*tariff.Quota{{
			InboundTag: "full-tunnel",
			LimitBytes: 20 * 1024 * mb,
			Window:     tariff.Window_WINDOW_MONTH,
			Action:     tariff.Action_ACTION_BLOCK,
		}},
	}
	if _, err := client.SetPolicy(ctx, &command.SetPolicyRequest{Policy: policy}); err != nil {
		t.Fatal(err)
	}

	got, err := client.GetPolicy(ctx, &command.GetPolicyRequest{Email: "alice@example"})
	if err != nil {
		t.Fatal(err)
	}
	p := got.GetPolicy()
	if p.GetDownlinkBps() != 20*mb || p.GetMaxDevices() != 3 {
		t.Fatalf("policy came back as %d B/s, %d devices", p.GetDownlinkBps(), p.GetMaxDevices())
	}
	if len(p.GetQuotas()) != 1 || p.GetQuotas()[0].GetInboundTag() != "full-tunnel" {
		t.Fatalf("quotas came back as %v", p.GetQuotas())
	}

	list, err := client.ListPolicies(ctx, &command.ListPoliciesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.GetPolicies()) != 1 {
		t.Fatalf("ListPolicies returned %d policies, want 1", len(list.GetPolicies()))
	}
}

// TestSetPoliciesReplaceConverges is the sync path a panel actually uses: push
// the roster, and let the server drop whoever is no longer on it.
func TestSetPoliciesReplaceConverges(t *testing.T) {
	client, _ := dial(t)
	ctx := context.Background()

	for _, e := range []string{"a@example", "b@example", "c@example"} {
		if _, err := client.SetPolicy(ctx, &command.SetPolicyRequest{
			Policy: &tariff.Policy{Email: e},
		}); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := client.SetPolicies(ctx, &command.SetPoliciesRequest{
		Policies: []*tariff.Policy{{Email: "a@example"}, {Email: "d@example"}},
		Replace:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetApplied() != 2 || resp.GetRemoved() != 2 {
		t.Fatalf("applied=%d removed=%d, want 2 and 2", resp.GetApplied(), resp.GetRemoved())
	}

	list, err := client.ListPolicies(ctx, &command.ListPoliciesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, p := range list.GetPolicies() {
		got[p.GetEmail()] = true
	}
	if len(got) != 2 || !got["a@example"] || !got["d@example"] {
		t.Fatalf("after a replacing sync the server holds %v, want exactly a@ and d@", got)
	}
}

func TestUsageReportsWhatWasSpent(t *testing.T) {
	client, m := dial(t)
	ctx := context.Background()

	if _, err := client.SetPolicy(ctx, &command.SetPolicyRequest{
		Policy: &tariff.Policy{Email: "u@example", DownlinkBps: 5 * mb},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AddUsage(ctx, &command.AddUsageRequest{
		Email: "u@example", InboundTag: "vless-in", Uplink: 100, Downlink: 900,
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := client.GetUsage(ctx, &command.GetUsageRequest{Email: "u@example"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetUsers()) != 1 {
		t.Fatalf("GetUsage returned %d users, want 1", len(resp.GetUsers()))
	}
	u := resp.GetUsers()[0]
	if u.GetUplink() != 100 || u.GetDownlink() != 900 {
		t.Fatalf("usage = %d/%d, want 100/900", u.GetUplink(), u.GetDownlink())
	}
	if u.GetEffectiveDownlinkBps() != 5*mb {
		t.Fatalf("effective downlink = %d, want %d", u.GetEffectiveDownlinkBps(), 5*mb)
	}

	if _, err := client.ResetUsage(ctx, &command.ResetUsageRequest{Email: "u@example"}); err != nil {
		t.Fatal(err)
	}
	if got := m.Usage("u@example").GetDownlink(); got != 0 {
		t.Fatalf("usage after reset = %d, want 0", got)
	}
}

func TestGetStatusSummarisesTheServer(t *testing.T) {
	client, _ := dial(t)
	ctx := context.Background()

	if _, err := client.SetServerQuota(ctx, &command.SetServerQuotaRequest{
		Quota: &tariff.ServerQuota{
			LimitBytes: 20 * 1024 * 1024 * mb, // 20 TB
			Window:     tariff.Window_WINDOW_MONTH,
			Action:     tariff.Action_ACTION_NOTIFY,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SetPolicy(ctx, &command.SetPolicyRequest{
		Policy: &tariff.Policy{Email: "s@example"},
	}); err != nil {
		t.Fatal(err)
	}

	st, err := client.GetStatus(ctx, &command.GetStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if st.GetUsers() != 1 {
		t.Fatalf("status reports %d users, want 1", st.GetUsers())
	}
	if q := st.GetServerQuota(); q == nil || q.GetLimitBytes() != 20*1024*1024*mb {
		t.Fatalf("status did not report the server allowance: %v", q)
	}
}

func TestStreamEventsDeliversAndFilters(t *testing.T) {
	client, m := dial(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.StreamEvents(ctx, &command.StreamEventsRequest{
		Kinds: []tariff.EventKind{tariff.EventKind_EVENT_USER_CONNECTED},
		Email: "wanted@example",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the subscription to be registered before generating events,
	// otherwise the test races the stream's setup.
	waitFor(t, func() bool { return m.Subscribers() == 1 })

	// Noise the filter must swallow: the wrong user, and the wrong kind.
	m.Attach("other@example", 0, "in", "1.1.1.1").Release()
	if err := m.SetPolicy(&tariff.Policy{Email: "wanted@example"}); err != nil {
		t.Fatal(err)
	}
	// The one event that should arrive.
	m.Attach("wanted@example", 0, "in", "2.2.2.2")

	resp, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	e := resp.GetEvent()
	if e.GetKind() != tariff.EventKind_EVENT_USER_CONNECTED || e.GetEmail() != "wanted@example" {
		t.Fatalf("filter let through %v for %q", e.GetKind(), e.GetEmail())
	}
	if e.GetDevice() != "2.2.2.2" {
		t.Fatalf("event device = %q, want the address that connected", e.GetDevice())
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}
