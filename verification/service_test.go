package verification

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Alexander4731/Vestrail/db"
	"github.com/Alexander4731/Vestrail/evidence"
)

func fixture(t *testing.T) (Service, int64) {
	t.Helper()
	dsn := os.Getenv("VESTRAIL_PG_DSN")
	if dsn == "" {
		t.Skip("VESTRAIL_PG_DSN required")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	fid, err := pg.AddFinding(0, 0, "fixture", "offline fixture", "low", "claim", "proof", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.DeleteFinding(fid) })
	return Service{Store: evidence.New(pg, nil, t.TempDir())}, fid
}

func bindFixture(t *testing.T, s Service, fid int64, body []byte) string {
	t.Helper()
	hash := fmt.Sprintf("%x", sha256.Sum256(body))
	path := filepath.Join(s.Store.Dir, "blobs", hash[:2], hash+".bin")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := db.TrafficEvidenceSnapshot{SourceTrafficID: fmt.Sprint(fid), URL: "https://fixture.invalid/resource", Method: "GET", Status: 200, ReqHash: hash, RespHash: hash, ReqLen: int64(len(body)), RespLen: int64(len(body))}
	snapshot.ID = db.TrafficSnapshotID(snapshot)
	err := s.Store.DB.WithEvidenceTx(t.Context(), func(tx *sql.Tx) error {
		if err := db.LockFindingEvidenceTx(t.Context(), tx, fid, nil); err != nil {
			return err
		}
		return db.AddFindingTrafficTx(tx, fid, []db.PreparedTrafficEvidence{{Ref: db.TrafficRef{TrafficID: snapshot.SourceTrafficID, Role: "proof"}, Snapshot: snapshot}})
	})
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIntegrityDoesNotProveVulnerability(t *testing.T) {
	s, fid := fixture(t)
	path := bindFixture(t, s, fid, []byte("stored local evidence"))
	a, created, err := s.Run(t.Context(), fid, "human:test", Request{RequestKey: "first", EvidenceVersion: 1, Verifier: "evidence_integrity"})
	if err != nil || !created || a.Assurance != "evidence_bound" || a.Outcome != "supported" {
		t.Fatalf("%+v %v %v", a, created, err)
	}
	f, _ := s.Store.DB.GetFinding(fid)
	if f.VerificationState != "candidate" || f.Status != db.FindingPending {
		t.Fatal("200 + complete file promoted claim")
	}
	again, created, err := s.Run(t.Context(), fid, "human:test", Request{RequestKey: "first", EvidenceVersion: 1, Verifier: "evidence_integrity"})
	if err != nil || created || again.ID != a.ID {
		t.Fatalf("retry: %+v %v %v", again, created, err)
	}
	if err = os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	a, _, err = s.Run(t.Context(), fid, "human:test", Request{RequestKey: "second", EvidenceVersion: 1, Verifier: "evidence_integrity"})
	if err != nil || a.Outcome != "inconclusive" || a.Assurance != "none" {
		t.Fatalf("tampered body: %+v %v", a, err)
	}
}

func TestIntegrityBudgetsSharedBodiesAndMetadata(t *testing.T) {
	t.Run("shared bodies", func(t *testing.T) {
		s, fid := fixture(t)
		bindFixture(t, s, fid, bytes.Repeat([]byte("x"), 1<<20))
		traffic, err := s.Store.DB.GetFindingTraffic(t.Context(), fid)
		if err != nil {
			t.Fatal(err)
		}
		snapshot := traffic.Bindings[0].Snapshot
		prepared := make([]db.PreparedTrafficEvidence, maxBindings)
		for i := range prepared {
			copy := snapshot
			copy.SourceTrafficID = fmt.Sprintf("%d-extra-%d", fid, i)
			copy.ID = db.TrafficSnapshotID(copy)
			prepared[i] = db.PreparedTrafficEvidence{Ref: db.TrafficRef{TrafficID: copy.SourceTrafficID, Role: "proof"}, Snapshot: copy}
		}
		add := func(items []db.PreparedTrafficEvidence) {
			t.Helper()
			if err := s.Store.DB.WithEvidenceTx(t.Context(), func(tx *sql.Tx) error {
				if err := db.LockFindingEvidenceTx(t.Context(), tx, fid, nil); err != nil {
					return err
				}
				return db.AddFindingTrafficTx(tx, fid, items)
			}); err != nil {
				t.Fatal(err)
			}
		}
		add(prepared[:maxBindings-1])
		a, _, err := s.Run(t.Context(), fid, "human:test", Request{RequestKey: "shared", EvidenceVersion: 2, Verifier: "evidence_integrity"})
		if err != nil || a.Assurance != "evidence_bound" {
			t.Fatalf("shared body was charged per binding: %+v %v", a, err)
		}
		var refs []json.RawMessage
		if err = json.Unmarshal(a.EvidenceRefs, &refs); err != nil || len(refs) != maxBindings {
			t.Fatal("body deduplication dropped binding references", err)
		}
		add(prepared[maxBindings-1:])
		a, _, err = s.Run(t.Context(), fid, "human:test", Request{RequestKey: "too-many", EvidenceVersion: 3, Verifier: "evidence_integrity"})
		if err != nil || a.Assurance != "none" || string(a.Limitations) != `["binding_limit"]` {
			t.Fatalf("binding cap failed: %+v %v", a, err)
		}
	})
	t.Run("metadata", func(t *testing.T) {
		s, fid := fixture(t)
		bindFixture(t, s, fid, []byte("local body"))
		traffic, err := s.Store.DB.GetFindingTraffic(t.Context(), fid)
		if err != nil {
			t.Fatal(err)
		}
		note := strings.Repeat("x", int(maxMetadataBytes)+1)
		if err = s.Store.DB.EditFindingTraffic(t.Context(), fid, traffic.Bindings[0].ID, 1, nil, &note, false, nil); err != nil {
			t.Fatal(err)
		}
		a, _, err := s.Run(t.Context(), fid, "human:test", Request{RequestKey: "large-metadata", EvidenceVersion: 2, Verifier: "evidence_integrity"})
		if err != nil || a.Assurance != "none" || string(a.Limitations) != `["metadata_limit"]` {
			t.Fatalf("metadata cap failed: %+v %v", a, err)
		}
	})
}

func TestMissingEvidenceAndUnboundReceipt(t *testing.T) {
	s, fid := fixture(t)
	for i, verifier := range []string{"evidence_integrity", "execution_receipt"} {
		req := Request{RequestKey: fmt.Sprint(i), Verifier: verifier}
		if i == 1 {
			req.ReceiptID = 9223372036854775806
		}
		a, _, err := s.Run(t.Context(), fid, "human:test", req)
		if err != nil || a.Outcome != "inconclusive" || a.Assurance != "none" {
			t.Fatalf("%+v %v", a, err)
		}
	}
	if _, _, err := s.Run(t.Context(), fid, "human:test", Request{RequestKey: "spoof", Verifier: "controlled_marker"}); err == nil {
		t.Fatal("unregistered verifier accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := s.Run(ctx, fid, "human:test", Request{RequestKey: "canceled", Verifier: "evidence_integrity"}); err == nil {
		t.Fatal("canceled run accepted")
	}
}
