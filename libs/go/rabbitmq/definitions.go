package rabbitmq

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// userName keeps a user's password file, rabbitmq-<name>, apart from every
// other file the local setup writes.
var userName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// password is what a user's password may contain: letters and digits, so a
// generated or rotated value needs no escaping anywhere it goes.
var password = regexp.MustCompile(`^[A-Za-z0-9]{32,}$`)

// DefinitionUsers returns the user names in a definitions template, after
// checking that each user's password is its own "@name@" placeholder, so the
// template, which is committed, can never carry a real password.
func DefinitionUsers(template []byte) ([]string, error) {
	var definitions struct {
		Users []struct {
			Name     string `json:"name"`
			Password string `json:"password"`
		} `json:"users"`
	}
	if err := json.Unmarshal(template, &definitions); err != nil {
		return nil, fmt.Errorf("definitions template: %w", err)
	}
	names := make([]string, len(definitions.Users))
	for i, u := range definitions.Users {
		if !userName.MatchString(u.Name) {
			return nil, fmt.Errorf("definitions template: user name %q must be lowercase letters, digits, '-' and '_'", u.Name)
		}
		if u.Password != "@"+u.Name+"@" {
			return nil, fmt.Errorf("definitions template: user %q must have the password placeholder %q", u.Name, "@"+u.Name+"@")
		}
		names[i] = u.Name
	}
	return names, nil
}

// RenderDefinitions fills each user's password into a definitions template,
// after DefinitionUsers' checks. Every user needs a password of at least 32
// letters and digits.
func RenderDefinitions(template []byte, passwords map[string]string) ([]byte, error) {
	if _, err := DefinitionUsers(template); err != nil {
		return nil, err
	}
	var definitions map[string]any
	if err := json.Unmarshal(template, &definitions); err != nil {
		return nil, err
	}
	users, _ := definitions["users"].([]any)
	for _, u := range users {
		user, _ := u.(map[string]any)
		name, _ := user["name"].(string)
		if !password.MatchString(passwords[name]) {
			return nil, fmt.Errorf("user %q needs a password of at least 32 letters and digits", name)
		}
		user["password"] = passwords[name]
	}
	return json.MarshalIndent(definitions, "", "  ")
}
