package projects

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestCreateIdempotentSurvivesStoreRestartAndConflicts(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "projects"))
	root := t.TempDir()
	first, created, err := store.CreateIdempotent(context.Background(), "operation-project-create", "fingerprint-1", root, "Original")
	if err != nil {
		t.Fatalf("first CreateIdempotent() error = %v", err)
	}
	if !created || first.ID == "" {
		t.Fatalf("first result = %#v, created=%v", first, created)
	}
	claimData, err := os.ReadFile(store.projectOperationClaimPath("operation-project-create"))
	if err != nil {
		t.Fatalf("read durable claim: %v", err)
	}
	if bytes.Contains(claimData, []byte(root)) {
		t.Fatal("durable project create claim leaked the absolute project root")
	}
	if bytes.Contains(claimData, []byte("Original")) {
		t.Fatal("durable project create claim leaked the display name")
	}
	outcomeData, err := os.ReadFile(store.projectOperationOutcomePath("operation-project-create"))
	if err != nil {
		t.Fatalf("read durable outcome: %v", err)
	}
	if bytes.Contains(outcomeData, []byte(root)) || bytes.Contains(outcomeData, []byte("Original")) {
		t.Fatal("durable project create outcome leaked root or display name")
	}

	restarted := NewStore(store.Root())
	retry, created, err := restarted.CreateIdempotent(context.Background(), "operation-project-create", "fingerprint-1", root, "Original")
	if err != nil {
		t.Fatalf("restart retry error = %v", err)
	}
	if !created || retry.ID != first.ID {
		t.Fatalf("restart retry = %#v, created=%v, want same durable identity and created=true", retry, created)
	}
	if _, _, err := restarted.CreateIdempotent(context.Background(), "operation-project-create", "fingerprint-2", root, "Original"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting operation fingerprint error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestCreateIdempotentUsesCanonicalRootAndDoesNotAdoptDifferentName(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "projects"))
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}
	first, created, err := store.CreateIdempotent(context.Background(), "operation-canonical", "fingerprint-canonical", root, "Canonical")
	if err != nil || !created {
		t.Fatalf("first canonical create = %#v/%v, want created", first, err)
	}
	duplicate, created, err := store.CreateIdempotent(context.Background(), "operation-other", "fingerprint-other", alias, "Other")
	if err != nil {
		t.Fatalf("canonical duplicate error = %v", err)
	}
	if created || duplicate.ID != first.ID || duplicate.DisplayName != "Canonical" {
		t.Fatalf("canonical duplicate = %#v, created=%v, want original project without rename", duplicate, created)
	}

	// A pending claim may only recover a row whose normalized display name
	// agrees. This models a crash before the outcome write and proves a
	// different independent create cannot be adopted by the old operation.
	canonicalRoot, err := CanonicalRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	claim := projectCreateClaim{
		Version: projectOperationVersion, Revision: 1, OperationID: "operation-pending",
		Fingerprint: "fingerprint-pending", ProjectID: projectIDForRoot(canonicalRoot),
		RootHash:        projectOperationHash(projectRootKey(canonicalRoot)),
		DisplayNameHash: projectOperationHash("Pending"), State: projectOperationPending,
	}
	if err := store.writeProjectCreateClaim(claim); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateIdempotent(context.Background(), claim.OperationID, claim.Fingerprint, root, "Pending"); !errors.Is(err, ErrOperationOutcomeUnknown) {
		t.Fatalf("pending different-name recovery error = %v, want ErrOperationOutcomeUnknown", err)
	}

	sameClaim := projectCreateClaim{
		Version: projectOperationVersion, Revision: 1, OperationID: "operation-pending-same",
		Fingerprint: "fingerprint-pending-same", ProjectID: first.ID,
		RootHash:        projectOperationHash(projectRootKey(canonicalRoot)),
		DisplayNameHash: projectOperationHash("Canonical"), State: projectOperationPending,
	}
	if err := store.writeProjectCreateClaim(sameClaim); err != nil {
		t.Fatal(err)
	}
	recovered, created, err := store.CreateIdempotent(context.Background(), sameClaim.OperationID, sameClaim.Fingerprint, alias, "Canonical")
	if err != nil || !created || recovered.ID != first.ID {
		t.Fatalf("same-name pending recovery = %#v, created=%v, error=%v", recovered, created, err)
	}
	recoveredAgain, created, err := store.CreateIdempotent(context.Background(), sameClaim.OperationID, sameClaim.Fingerprint, root, "Canonical")
	if err != nil || !created || recoveredAgain.ID != first.ID {
		t.Fatalf("recovered operation retry = %#v, created=%v, error=%v", recoveredAgain, created, err)
	}
}

func TestCreateIdempotentOutcomeWriteFailureKeepsPendingClaimAndConflict(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "projects"))
	root := t.TempDir()
	originalWriter := store.writeProjectOperationRecord
	store.writeProjectOperationRecord = func(path string, data []byte) error {
		if strings.HasSuffix(path, ".outcome.json") {
			return errors.New("injected outcome write failure")
		}
		return originalWriter(path, data)
	}
	_, _, err := store.CreateIdempotent(context.Background(), "operation-fault", "fingerprint-fault", root, "Fault display")
	if !errors.Is(err, ErrOperationOutcomeUnknown) {
		t.Fatalf("faulted outcome write error = %v, want ErrOperationOutcomeUnknown", err)
	}
	claim, err := store.readProjectCreateClaim("operation-fault")
	if err != nil {
		t.Fatalf("read retained pending claim: %v", err)
	}
	if claim == nil || claim.State != projectOperationPending || claim.Revision != 1 {
		t.Fatalf("retained claim = %#v, want pending revision 1", claim)
	}
	if _, _, err := store.CreateIdempotent(context.Background(), "operation-fault", "different-fingerprint", root, "Fault display"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different fingerprint after failed outcome = %v, want ErrIdempotencyConflict", err)
	}
	projects, err := store.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 {
		t.Fatalf("project rows after outcome fault = %d, want exactly one", len(projects))
	}
}

func TestConcurrentProjectCreatesShareOneDeterministicRootIdentity(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "projects")
	root := t.TempDir()
	const workers = 16
	type result struct {
		project Project
		created bool
		err     error
	}
	results := make(chan result, workers*2)
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(2)
		go func(index int) {
			defer wait.Done()
			project, created, err := NewStore(storeRoot).Create(root, fmt.Sprintf("ordinary-%d", index))
			results <- result{project: project, created: created, err: err}
		}(i)
		go func(index int) {
			defer wait.Done()
			project, created, err := NewStore(storeRoot).CreateIdempotent(context.Background(), fmt.Sprintf("operation-concurrent-%d", index), fmt.Sprintf("fingerprint-%d", index), root, fmt.Sprintf("durable-%d", index))
			results <- result{project: project, created: created, err: err}
		}(i)
	}
	wait.Wait()
	close(results)

	var identity string
	createdCount := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent create error = %v", result.err)
		}
		if identity == "" {
			identity = result.project.ID
		}
		if result.project.ID != identity {
			t.Fatalf("concurrent project identity = %q, want %q", result.project.ID, identity)
		}
		if result.created {
			createdCount++
		}
	}
	if createdCount == 0 {
		t.Fatal("concurrent create did not report a creator")
	}
	projects, err := NewStore(storeRoot).ListAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].ID != identity {
		t.Fatalf("durable project rows = %#v, want one row %q", projects, identity)
	}
}
