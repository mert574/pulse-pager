//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"pulse/internal/domain"
	"pulse/internal/events"
	"pulse/internal/notify"
	"pulse/internal/store"
)

// recordingEmailMailer captures every recipient the platform mailer is asked to send to,
// so the test can assert the Team email channel reached exactly the org's members
// and nobody else.
type recordingEmailMailer struct {
	mu sync.Mutex
	to []string
}

func (m *recordingEmailMailer) Send(_ context.Context, msg notify.Mail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.to = append(m.to, msg.To)
	return nil
}

func (m *recordingEmailMailer) recipients() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]string(nil), m.to...)
	sort.Strings(out)
	return out
}

// TestNotifierEmailChannel drives the Team email channel end to end through the real
// notifier Runner against a real Postgres (RLS in force): a down event is dispatched
// to an "email" channel whose config holds two member ids, and the platform mailer
// must be asked to send to exactly those two members' addresses and no external one.
// Then one member is removed and a second incident is fired: the resolver join drops
// the removed member at send time, so only the remaining member gets mail.
func TestNotifierEmailChannel(t *testing.T) {
	ctx := context.Background()

	pgC, err := postgres.Run(ctx, "postgres:16",
		postgres.WithDatabase("pulse"),
		postgres.WithUsername("pulse"),
		postgres.WithPassword("pulse"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	defer func() { _ = pgC.Terminate(ctx) }()

	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	if err := store.ApplySchema(ctx, pool); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	var orgID int64
	if err := pool.QueryRow(ctx, "INSERT INTO organizations(name, slug) VALUES('Email Org', 'email-org') RETURNING id").Scan(&orgID); err != nil {
		t.Fatal(err)
	}

	// Two users, both active members of the org, plus a third user who is NOT a member
	// (to prove the resolver never reaches outside the org's membership).
	mkUser := func(email string) int64 {
		u := &domain.User{Email: email, EmailVerified: true, Name: email}
		id, err := pool.CreateUser(ctx, u)
		if err != nil {
			t.Fatalf("create user %s: %v", email, err)
		}
		return id
	}
	mkMember := func(userID int64, role domain.Role) {
		if _, err := pool.CreateMembership(ctx, &domain.Membership{OrgID: orgID, UserID: userID, Role: role}); err != nil {
			t.Fatalf("create membership: %v", err)
		}
	}
	aliceID := mkUser("alice@org.test")
	bobID := mkUser("bob@org.test")
	outsiderID := mkUser("outsider@elsewhere.test")
	mkMember(aliceID, domain.RoleOwner)
	mkMember(bobID, domain.RoleMember)
	_ = outsiderID // intentionally not a member of this org

	// One "email" channel selecting alice + bob (member ids as strings, the wire shape).
	insertChannel := func(name string, cfg map[string]any) int64 {
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var id int64
		if err := pool.QueryRow(ctx,
			"INSERT INTO channels(org_id, name, type, config, enabled) VALUES($1,$2,'email',$3,true) RETURNING id",
			orgID, name, raw).Scan(&id); err != nil {
			t.Fatalf("insert email channel: %v", err)
		}
		return id
	}
	emailID := insertChannel("team mail", map[string]any{
		"members": []any{intToStr(aliceID), intToStr(bobID)},
	})

	mon := &domain.Monitor{
		OrgID: orgID, Name: "email mon", URL: "https://example.test", Method: "GET",
		ExpectedStatusCodes: "200", TimeoutSeconds: 5, IntervalSeconds: 60, Enabled: true,
		FailureThreshold: 1, Regions: []string{"eu-central"}, DownPolicy: domain.DownPolicyQuorum,
		ChannelIDs: []int64{emailID},
	}
	if _, err := pool.CreateMonitor(ctx, mon); err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	now := time.Now().UTC()
	mkIncident := func() int64 {
		id, err := pool.CreateIncident(ctx, &domain.Incident{
			OrgID: orgID, MonitorID: mon.ID, StartedAt: now, CauseReason: domain.ReasonStatusMismatch,
		})
		if err != nil {
			t.Fatalf("create incident: %v", err)
		}
		return id
	}
	mkEvent := func(incID int64, evType string) []byte {
		ev := events.NotifyEvent{
			OrgID: orgID, MonitorID: mon.ID, IncidentID: incID, EventType: evType,
			DedupKey: notify.DedupKey(incID, evType), MonitorName: mon.Name,
			MonitorURL: mon.URL, MonitorMethod: "GET", IncidentStartedAt: now,
			Check:  domain.CheckResult{MonitorID: mon.ID, OrgID: orgID, CheckedAt: now, Healthy: evType != notify.EventDown},
			SentAt: now,
		}
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	mailer := &recordingEmailMailer{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	runOnce := func(raw []byte) {
		cons := &directConsumer{recs: [][]byte{raw}, delivered: make(chan struct{})}
		mgr := notify.NewManager(nil, log)
		mgr.SetRetryPolicy(2, func(int) time.Duration { return time.Millisecond })
		// The pool is the member-email resolver; the recording mailer is the sink.
		mgr.SetEmailDeps(mailer, pool)
		runner := notify.NewRunner(mgr, notify.Default(), pool, newMemCache(), cons, log)
		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() { _ = runner.Run(runCtx) }()
		select {
		case <-cons.delivered:
		case <-time.After(20 * time.Second):
			t.Fatal("notifier did not process the email event in time")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// One incident stays open for the whole test (uniq_incident_open allows only one
	// per monitor), reused across both sends; each send uses a fresh dedup cache so the
	// second one is not suppressed.
	incID := mkIncident()

	// First send (down): both members get mail, no external address.
	runOnce(mkEvent(incID, notify.EventDown))
	got := mailer.recipients()
	want := []string{"alice@org.test", "bob@org.test"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("first send recipients = %v, want %v (and never outsider@elsewhere.test)", got, want)
	}

	// Remove bob from the org, then fire the recovery for the same incident (a distinct
	// dedup key, so it is not suppressed): the resolver join drops the removed member at
	// send time, so only alice gets the recovery mail.
	if _, err := pool.RemoveMember(ctx, orgID, bobID); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	mailer.mu.Lock()
	mailer.to = nil
	mailer.mu.Unlock()

	runOnce(mkEvent(incID, notify.EventRecovery))
	got = mailer.recipients()
	if len(got) != 1 || got[0] != "alice@org.test" {
		t.Fatalf("after removing bob, recipients = %v, want only alice@org.test", got)
	}
}

// intToStr renders an int64 member id as the string id shape the channel config and
// the API use (RFC-012 ids are strings).
func intToStr(id int64) string {
	return strconv.FormatInt(id, 10)
}
