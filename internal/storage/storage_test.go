package storage

import (
	"strings"
	"testing"
	"time"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open("sqlite", t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAccountUpsertLifecycle(t *testing.T) {
	s := openStore(t)
	seeds := []AccountSeed{
		{Email: "Sender@Example.com", Password: "secret1"},
		{Email: "b@example.com", Password: "secret2", AllowedFrom: []string{"news@example.com", "news@example.com"}},
	}
	sum, err := s.UpsertAccounts(seeds)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Created) != 2 {
		t.Fatalf("created = %v, want 2", sum.Created)
	}

	acc, ok, err := s.GetAccount("sender@example.com")
	if err != nil || !ok {
		t.Fatalf("GetAccount: ok=%v err=%v", ok, err)
	}
	if !VerifyPassword(acc.PasswordHash, "secret1") {
		t.Error("password hash does not verify")
	}
	if VerifyPassword(acc.PasswordHash, "wrong") {
		t.Error("wrong password should not verify")
	}
	if !acc.AllowsFrom("sender@example.com") {
		t.Error("allowed_from should include the own email")
	}
	accB, _, _ := s.GetAccount("b@example.com")
	if len(accB.AllowedFrom) != 2 || !accB.AllowsFrom("news@example.com") {
		t.Errorf("allowed_from wrong: %v", accB.AllowedFrom)
	}

	// Unchanged seed: same password and allowed list => untouched.
	sum2, err := s.UpsertAccounts([]AccountSeed{{Email: "b@example.com", Password: "secret2", AllowedFrom: []string{"news@example.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sum2.Unchanged) != 1 {
		t.Errorf("unchanged = %v, want 1", sum2.Unchanged)
	}

	// Updated seed: new password.
	sum3, _ := s.UpsertAccounts([]AccountSeed{{Email: "b@example.com", Password: "secret3"}})
	if len(sum3.Updated) != 1 {
		t.Errorf("updated = %v, want 1", sum3.Updated)
	}
	accB2, _, _ := s.GetAccount("b@example.com")
	if !VerifyPassword(accB2.PasswordHash, "secret3") {
		t.Error("password was not updated")
	}

	if err := s.DeleteAccount("b@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetAccount("b@example.com"); ok {
		t.Error("account should be gone after delete")
	}
	list, err := s.ListAccounts()
	if err != nil || len(list) != 1 {
		t.Errorf("ListAccounts = %d, err=%v", len(list), err)
	}
}

func TestDKIMUpsertLifecycle(t *testing.T) {
	s := openStore(t)
	sum, err := s.UpsertDKIM([]DKIMKey{{Domain: "Example.com", Selector: "mail", KeyData: "PEM1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Created) != 1 {
		t.Fatalf("created = %v", sum.Created)
	}
	k, ok, err := s.GetDKIM("example.com")
	if err != nil || !ok {
		t.Fatalf("GetDKIM: ok=%v err=%v", ok, err)
	}
	if k.Selector != "mail" || k.KeyData != "PEM1" {
		t.Errorf("dkim row wrong: %+v", k)
	}
	// Same content => unchanged.
	sumSame, err := s.UpsertDKIM([]DKIMKey{{Domain: "example.com", Selector: "mail", KeyData: "PEM1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sumSame.Unchanged) != 1 {
		t.Errorf("unchanged = %v, want 1", sumSame.Unchanged)
	}
	// Changed content => updated.
	if _, err := s.UpsertDKIM([]DKIMKey{{Domain: "example.com", Selector: "mail2", KeyData: "PEM2"}}); err != nil {
		t.Fatal(err)
	}
	k2, _, _ := s.GetDKIM("example.com")
	if k2.KeyData != "PEM2" {
		t.Error("dkim upsert did not update")
	}
	if err := s.DeleteDKIM("example.com"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetDKIM("example.com"); ok {
		t.Error("dkim should be gone after delete")
	}
}

func TestQueueLifecycle(t *testing.T) {
	s := openStore(t)
	id, err := s.Enqueue("sender@example.com", []string{"dest@example.com"}, []byte("Subject: x\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("empty id")
	}

	msgs, err := s.NextDue(time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("NextDue = %d, want 1", len(msgs))
	}
	m := msgs[0]
	if m.ID != id || m.From != "sender@example.com" || string(m.Data) == "" {
		t.Errorf("message wrong: %+v", m)
	}

	// A claimed message is not returned again until the lease expires.
	again, _ := s.NextDue(time.Now(), 10)
	if len(again) != 0 {
		t.Fatal("claimed message returned twice")
	}
	// After the lease expires it becomes due again (at-least-once).
	after, _ := s.NextDue(time.Now().Add(LeaseTimeout+time.Minute), 10)
	if len(after) != 1 {
		t.Fatalf("message should be due after the lease, got %d", len(after))
	}

	// Retry scheduling: persist next attempt, release, and it must not be due.
	m.Attempts = 1
	m.NextAttempt = time.Now().Add(time.Hour)
	m.LastError = "temporary"
	if err := s.Persist(m); err != nil {
		t.Fatal(err)
	}
	s.Release(id)
	notDue, _ := s.NextDue(time.Now(), 10)
	if len(notDue) != 0 {
		t.Fatal("message with future next_attempt should not be due")
	}
	dueLater, _ := s.NextDue(time.Now().Add(2*time.Hour), 10)
	if len(dueLater) != 1 {
		t.Fatal("message should be due after next_attempt")
	}

	// Success removes it.
	dueLater[0].To = []string{}
	dueLater[0].Permanent = map[string]string{"dest@example.com": "moved on"}
	if err := s.Succeed(dueLater[0].ID); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Stats(time.Now())
	if st.Queued != 0 {
		t.Errorf("queued = %d, want 0", st.Queued)
	}
}

func TestDeadLetterAndRequeue(t *testing.T) {
	s := openStore(t)
	s.Enqueue("f@example.com", []string{"t@example.com"}, []byte("x"))
	msgs, _ := s.NextDue(time.Now(), 10)
	if err := s.DeadLetter(msgs[0].ID, "permanent failure"); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Stats(time.Now())
	if st.Dead != 1 || st.Queued != 0 {
		t.Fatalf("stats after dead-letter: %+v", st)
	}
	list, _ := s.ListMessages(StatusDead, 10)
	if len(list) != 1 || !strings.Contains(list[0].LastError, "permanent") {
		t.Errorf("dead list wrong: %+v", list)
	}
	if err := s.RequeueDead(msgs[0].ID); err != nil {
		t.Fatal(err)
	}
	st2, _ := s.Stats(time.Now())
	if st2.Dead != 0 || st2.Queued != 1 {
		t.Fatalf("stats after requeue: %+v", st2)
	}
	if err := s.RequeueDead("does-not-exist"); err == nil {
		t.Error("requeue of a nonexistent/dead message should fail")
	}
}

func TestDeadLetterCapPrunesOldest(t *testing.T) {
	s := openStore(t)
	s.SetDeadMax(2)
	for i := 0; i < 3; i++ {
		s.Enqueue("f@example.com", []string{"t@example.com"}, []byte("x"))
	}
	// Dead-letter every message the way the worker does (claim then resolve).
	for {
		msgs, _ := s.NextDue(time.Now(), 10)
		if len(msgs) == 0 {
			break
		}
		for _, m := range msgs {
			s.DeadLetter(m.ID, "boom")
		}
	}
	st, _ := s.Stats(time.Now())
	if st.Dead != 2 || st.Queued != 0 {
		t.Fatalf("stats = %+v, want 2 dead (oldest pruned)", st)
	}
	// The cap is still enforced on further dead letters.
	s.Enqueue("f2@example.com", []string{"t@example.com"}, []byte("y"))
	msgs, _ := s.NextDue(time.Now(), 10)
	s.DeadLetter(msgs[0].ID, "boom again")
	st2, _ := s.Stats(time.Now())
	if st2.Dead != 2 {
		t.Fatalf("dead after more = %d, want 2", st2.Dead)
	}
}
