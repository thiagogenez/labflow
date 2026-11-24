package rabbitmq

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderDefinitions(t *testing.T) {
	good := strings.Repeat("a", 32)
	for _, tc := range []struct {
		name, users string
		passwords   map[string]string
		wantErr     string
	}{
		{name: "placeholders", users: `[{"name": "admin", "password": "@admin@"}, {"name": "worker2", "password": "@worker2@"}]`,
			passwords: map[string]string{"admin": good, "worker2": strings.Repeat("b", 32)}},
		{name: "a committed password", users: `[{"name": "worker2", "password": "hunter2"}]`,
			passwords: map[string]string{"worker2": good}, wantErr: `user "worker2" must have the password placeholder`},
		{name: "another user's placeholder", users: `[{"name": "admin", "password": "@order-api@"}, {"name": "order-api", "password": "@order-api@"}]`,
			passwords: map[string]string{"admin": good, "order-api": good}, wantErr: `user "admin" must have the password placeholder`},
		{name: "a name that collides with a file", users: `[{"name": "definitions.json", "password": "@definitions.json@"}]`,
			passwords: map[string]string{"definitions.json": good}, wantErr: `user name "definitions.json" must be`},
		{name: "no password", users: `[{"name": "admin", "password": "@admin@"}]`, wantErr: `user "admin" needs a password`},
		{name: "a password JSON would need to escape", users: `[{"name": "admin", "password": "@admin@"}]`,
			passwords: map[string]string{"admin": `@"` + good}, wantErr: `user "admin" needs a password`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := RenderDefinitions([]byte(`{"users": `+tc.users+`}`), tc.passwords)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("RenderDefinitions error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			var got struct {
				Users []struct{ Name, Password string }
			}
			if err != nil || json.Unmarshal(out, &got) != nil {
				t.Fatalf("RenderDefinitions: %v", err)
			}
			for _, u := range got.Users {
				if u.Password != tc.passwords[u.Name] {
					t.Errorf("user %s did not get its own password", u.Name)
				}
			}
		})
	}
}
