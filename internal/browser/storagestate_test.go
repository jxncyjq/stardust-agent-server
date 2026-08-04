package browser

import "testing"

func TestStorageStateRoundTripJSON(t *testing.T) {
	cookies := []CookieState{
		{Name: "sid", Value: "abc", Domain: "example.com", Path: "/", Secure: true},
		{Name: "pref", Value: "dark", Domain: "example.com", Path: "/"},
	}
	js, err := marshalStorageState(cookies)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back, err := unmarshalStorageState(js)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back) != 2 || back[0].Name != "sid" || back[0].Value != "abc" || !back[0].Secure {
		t.Fatalf("roundtrip mismatch: %+v", back)
	}
}

func TestUnmarshalEmptyStorageState(t *testing.T) {
	back, err := unmarshalStorageState("")
	if err != nil || len(back) != 0 {
		t.Fatalf("empty should give nil, got %+v err=%v", back, err)
	}
}
