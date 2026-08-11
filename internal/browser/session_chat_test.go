package browser

import "testing"

func TestSessionStore_BindAndFindByChatSession(t *testing.T) {
	st := NewSessionStore()
	s1 := st.Create("task-1")
	st.BindChat(s1.ID, "chat-A")

	got, ok := st.FindByChatSession("chat-A")
	if !ok || got.ID != s1.ID {
		t.Fatalf("FindByChatSession(chat-A) = %v, ok=%v; want %s", got, ok, s1.ID)
	}
	if got.ChatSessionID != "chat-A" {
		t.Fatalf("session ChatSessionID = %q, want chat-A", got.ChatSessionID)
	}
}

func TestSessionStore_FindByChatSession_UnknownAndEmpty(t *testing.T) {
	st := NewSessionStore()
	if _, ok := st.FindByChatSession("nope"); ok {
		t.Fatalf("unknown chat must return ok=false")
	}
	// 空 chatID：既不绑定也不命中。
	s := st.Create("task-1")
	st.BindChat(s.ID, "")
	if _, ok := st.FindByChatSession(""); ok {
		t.Fatalf("empty chat id must not be indexed")
	}
}

func TestSessionStore_DeleteRemovesChatBinding(t *testing.T) {
	st := NewSessionStore()
	s := st.Create("task-1")
	st.BindChat(s.ID, "chat-A")
	st.Delete(s.ID)
	if _, ok := st.FindByChatSession("chat-A"); ok {
		t.Fatalf("after Delete, chat-A binding must be gone")
	}
}

func TestSessionStore_RebindChatToNewSession(t *testing.T) {
	st := NewSessionStore()
	s1 := st.Create("task-1")
	st.BindChat(s1.ID, "chat-A")
	// 同 chat 绑到新会话（如旧会话彻底关闭后重开）。
	s2 := st.Create("task-2")
	st.BindChat(s2.ID, "chat-A")
	got, ok := st.FindByChatSession("chat-A")
	if !ok || got.ID != s2.ID {
		t.Fatalf("FindByChatSession(chat-A) = %v; want %s (latest binding)", got, s2.ID)
	}
	// 删旧会话不应清掉已指向新会话的绑定。
	st.Delete(s1.ID)
	if got, ok := st.FindByChatSession("chat-A"); !ok || got.ID != s2.ID {
		t.Fatalf("deleting old session wrongly cleared chat-A binding to new session")
	}
}
