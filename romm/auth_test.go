package romm

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestMissingSyncScopes(t *testing.T) {
	tests := []struct {
		name string
		have []string
		want []string
	}{
		{
			name: "all present plus extra",
			have: []string{"assets.read", "assets.write", "devices.read", "devices.write", "extra"},
			want: nil,
		},
		{
			name: "missing device scopes",
			have: []string{"assets.read", "assets.write"},
			want: []string{"devices.read", "devices.write"},
		},
		{
			name: "empty",
			have: nil,
			want: []string{"assets.read", "assets.write", "devices.read", "devices.write"},
		},
		{
			name: "all missing because unrelated scopes only",
			have: []string{"roms.read", "platforms.read"},
			want: []string{"assets.read", "assets.write", "devices.read", "devices.write"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MissingSyncScopes(tt.have); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("MissingSyncScopes(%v) = %v, want %v", tt.have, got, tt.want)
			}
		})
	}
}

// TestTokenExchangeResponseParse confirms the wire shape of a client-token exchange
// reply decodes into TokenExchangeResponse — the synthetic stand-in for a real
// pairing (no real code is minted in tests; --pair is validated live against a real RomM server
// at the device).
func TestTokenExchangeResponseParse(t *testing.T) {
	const body = `{
		"raw_token": "tok_abc123",
		"name": "Lodor - Mini Flip",
		"scopes": ["assets.read", "assets.write", "devices.read", "devices.write"],
		"expires_at": "2027-06-24T00:00:00Z"
	}`
	var got TokenExchangeResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.RawToken != "tok_abc123" {
		t.Errorf("RawToken = %q, want %q", got.RawToken, "tok_abc123")
	}
	if got.Name != "Lodor - Mini Flip" {
		t.Errorf("Name = %q", got.Name)
	}
	if len(got.Scopes) != 4 {
		t.Errorf("Scopes = %v, want 4", got.Scopes)
	}
	if miss := MissingSyncScopes(got.Scopes); miss != nil {
		t.Errorf("MissingSyncScopes on full grant = %v, want nil", miss)
	}
}

// TestRegisterDeviceResponseParse confirms POST /api/devices' reply (server returns
// the id under "device_id") maps onto Device.ID, and that a future "id"-keyed reply
// also parses.
func TestRegisterDeviceResponseParse(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"device_id key", `{"device_id":"dev-uuid-1","name":"Mini Flip"}`, "dev-uuid-1"},
		{"id key fallback", `{"id":"dev-uuid-2","name":"Mini Flip"}`, "dev-uuid-2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp registerDeviceResponse
			if err := json.Unmarshal([]byte(tc.body), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := resp.device().ID; got != tc.want {
				t.Errorf("device().ID = %q, want %q", got, tc.want)
			}
		})
	}
}
