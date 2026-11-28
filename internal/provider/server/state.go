package server

import (
	"errors"
	"time"

	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/provider"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
)

// State of a provider in one to three words and as the sentence its page starts with, by the one function every page, command and message
// uses: what it needs while it can't be used, Not signed in or Not connected while no credential works, the earliest an account frees up
// while each reached its limit; Signed in, Connected or Ready once it is ready. While it is not ready the sentence is the one thing wrong
// and the one thing to do, short enough for the line under the page's first
func providerState(instance provider.Instance, message *providerv1.Provider, credential credentials.Credential, unready error) (string, string) {
	label := instance.Label()
	standing := ""
	if unready != nil {
		standing = unready.Error()
		if worded, ok := errors.AsType[*credentials.Unready](unready); ok && worded.Short != "" {
			standing = worded.Short
		}
	}
	switch {
	case !instance.Known:
		return "Needs its plugin", label + " is a kind of provider, " + instance.Spec.Name + ", that no plugin of icebath knows."
	case !message.GetUsable() && instance.Own && message.GetBaseUrl() == "":
		return "Needs an address", label + " needs the address of its server."
	case !message.GetUsable():
		return "Needs a newer icebath", standing
	case unready != nil:
		if until := freed(message.GetAccounts()); !until.IsZero() {
			return "Limit reached until " + clock(until), standing
		}
		if instance.Spec.SignIn {
			return "Not signed in", standing
		}
		return "Not connected", standing
	case credential.APIKey == "" && credential.Sign == nil:
		return "Ready", label + " needs no account."
	}
	for _, account := range message.GetAccounts() {
		if account.GetName() != message.GetUsing() {
			continue
		}
		words := "Connected"
		if signedIn(account.GetAuth(), account.GetProgram()) {
			words = "Signed in"
		}
		if identity := account.GetIdentity(); identity != "" {
			return words, "Signed in as " + identity + "."
		}
		return words, "Connected with the account " + account.GetTitle() + "."
	}
	switch {
	case credential.Origin == credentials.External && credential.Identity != "":
		return "Signed in", "Signed in as " + credential.Identity + " through " + credential.Source + "."
	case credential.Origin == credentials.External:
		return "Signed in", "Signed in through " + credential.Source + "."
	case credential.Origin == credentials.Environment:
		return "Connected", "Connected with the API key in " + credential.Source + "."
	}
	return "Connected", "Connected."
}

// State of an account in one to three words and as the sentence its page starts with, by the one function every page and command uses:
// Signed in or Connected, Not signed in or Not connected, then in use and the limit it reached when they apply
func accountState(instance provider.Instance, account *providerv1.Account, credential credentials.Credential) (string, string) {
	signs := signedIn(account.GetAuth(), account.GetProgram()) || account.GetAuth() == "" && instance.Spec.SignIn && account.GetKeyEnv() == ""
	words, standing := "Connected", ""
	switch {
	case account.GetStatus() == credentials.Unavailable || account.GetStatus() == credentials.NeedsSignIn || account.GetStatus() == credentials.Gone:
		words = map[bool]string{true: "Not signed in", false: "Not connected"}[signs]
		standing = words + "."
	case credential.Origin == credentials.External && credential.Identity != "":
		words, standing = "Signed in", "Signed in as "+credential.Identity+" through "+credential.Source+"."
	case credential.Origin == credentials.External:
		words, standing = "Signed in", "Signed in through "+credential.Source+"."
	case credential.Origin == credentials.Environment:
		standing = "Uses the API key in " + credential.Source + "."
	case signs && credential.Identity != "":
		words, standing = "Signed in", "Signed in as "+credential.Identity+"."
	case signs:
		words, standing = "Signed in", "Signed in."
	default:
		standing = "Uses an API key that icebath keeps."
	}
	if account.GetActive() {
		words += ", in use"
	}
	if resets := account.GetResets(); account.GetStatus() == credentials.Limited && resets != nil {
		words += ", limit reached until " + clock(resets.AsTime())
	}
	return words, standing
}

// Whether a credential signs in rather than sends a key: a sign-in of the provider, or another program's
func signedIn(auth, program string) bool {
	return auth == string(provider.AuthOAuth) || program != ""
}

// When the first of the accounts frees up once each reached its limit, zero when one has none or the time isn't known
func freed(accounts []*providerv1.Account) time.Time {
	var first time.Time
	for _, account := range accounts {
		resets := account.GetResets()
		if account.GetStatus() != credentials.Limited || resets == nil {
			return time.Time{}
		}
		if at := resets.AsTime(); first.IsZero() || at.Before(first) {
			first = at
		}
	}
	return first
}

// Moment as a person reads it: the time alone today, else the day and the time
func clock(at time.Time) string {
	local, now := at.Local(), time.Now()
	if local.Year() == now.Year() && local.YearDay() == now.YearDay() {
		return local.Format("15:04")
	}
	return local.Format("Jan 2") + " at " + local.Format("15:04")
}
