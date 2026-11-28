package store

import (
	"errors"
	"testing"
)

// The skill system's store half (the design log's SKILL SYSTEM +
// WORKSPACE LOADING sections): the v10 conversations.workspace_id
// pointer (its default, the setter, the unknown-conversation 404), the
// workspace-delete pointer cascade (a dangling pointer would 404 the
// .SKILLS read), and the global-skill hard delete.

func TestConversationWorkspacePointer(t *testing.T) {
	s := openTestStore(t)
	c, err := s.CreateConversation("hello", "m1")
	if err != nil {
		t.Fatal(err)
	}
	// v10's default: no workspace.
	if c.WorkspaceID != "" {
		t.Fatalf("new conversation workspace_id = %q, want empty (the v10 default)", c.WorkspaceID)
	}
	if err := s.SetConversationWorkspace(c.ID, "ws1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetConversation(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkspaceID != "ws1" {
		t.Fatalf("workspace_id = %q, want ws1", got.WorkspaceID)
	}
	if err := s.SetConversationWorkspace(c.ID, ""); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetConversation(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkspaceID != "" {
		t.Fatalf("after clear = %q, want empty", got.WorkspaceID)
	}
	if err := s.SetConversationWorkspace("no-such-conv", "ws1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown conversation: err = %v, want ErrNotFound", err)
	}
}

func TestWorkspaceDeleteClearsConversationPointers(t *testing.T) {
	s := openTestStore(t)
	ws, err := s.CreateWorkspace("w", "d", "/tmp/w")
	if err != nil {
		t.Fatal(err)
	}
	c1, err := s.CreateConversation("a", "m1")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := s.CreateConversation("b", "m1")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*Conversation{c1, c2} {
		if err := s.SetConversationWorkspace(c.ID, ws.ID); err != nil {
			t.Fatal(err)
		}
	}
	// The third conversation is unpointed — it must survive untouched.
	c3, err := s.CreateConversation("c", "m1")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteWorkspace(ws.ID); err != nil {
		t.Fatal(err)
	}
	for i, c := range []*Conversation{c1, c2} {
		got, err := s.GetConversation(c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.WorkspaceID != "" {
			t.Fatalf("conversation %d workspace_id = %q after the workspace delete, want cleared", i+1, got.WorkspaceID)
		}
	}
	got, err := s.GetConversation(c3.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkspaceID != "" {
		t.Fatalf("unpointed conversation touched: %q", got.WorkspaceID)
	}
	// Deleting the (gone) workspace again is a 404, not a silent no-op.
	if err := s.DeleteWorkspace(ws.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: err = %v, want ErrNotFound", err)
	}
}

func TestDeleteSkill(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateSkill("deploy-cli", "deploys the app", "run the deploy script", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSkill("deploy-cli"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSkillByName("deploy-cli"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteSkill("deploy-cli"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: err = %v, want ErrNotFound", err)
	}
}
