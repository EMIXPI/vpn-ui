package service

import (
	"encoding/json"
	"strings"

	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/util/common"

	"gorm.io/gorm"
)

// The projection: rendering an Account plus one AccountInbound membership back
// into the inbound's settings.clients entry.
//
// This is the single writer of settings.clients once the accounts layer is
// authoritative, and it exists so that EVERY other consumer can stay exactly as
// it is. RADIUS, the slot allocator, all eleven daemon config writers, the
// routing translator and GetXrayConfig parse settings.clients and none of them
// learn about accounts.
//
// Two rules make this safe on a live panel:
//
//  1. OVERLAY, NEVER REBUILD. Rendering starts from the membership's stored copy
//     of the original entry and writes only the keys the account layer owns. A
//     rebuild-from-modelled-fields would silently drop wg-c/awg per-device
//     keypairs (clients[].devices[], which model.Client does not model at all)
//     and GRE pinned peers, invalidating client configs already installed on
//     users' devices.
//  2. SPLICE IN PLACE, NEVER REORDER. An entry is matched by email and updated
//     where it sits. Position is load-bearing: an account with no explicit slot
//     falls back to its INDEX in clients[] (slotOr), so reordering or compacting
//     the array moves live sessions onto other accounts' tunnel addresses.

// accountOwnedKeys are the settings-JSON keys the account row is authoritative
// for. Everything else in an entry belongs to the membership or the protocol and
// is passed through untouched.
//
// Credential keys are deliberately NOT in this list: which of them an account
// owns depends on the protocol, so they are applied by applyAccountCredential.
var accountOwnedKeys = []string{
	"email", "subId", "totalGB", "expiryTime", "enable",
	"reset", "limitIp", "tgId", "comment",
}

// applyAccountCredential writes the credential fields the given protocol keys on.
//
// The mapping mirrors buildTargetClientFromSource's switch, which is the tested
// answer to "what credential does protocol P need". Keep the two in step.
func applyAccountCredential(entry map[string]any, account *model.Account, protocol model.Protocol) {
	switch protocol {
	case model.VMESS:
		entry["id"] = account.UUID
		entry["security"] = account.Security
	case model.VLESS:
		entry["id"] = account.UUID
	case model.Trojan, model.Shadowsocks, model.ANYTLS:
		entry["password"] = account.Password
	case model.NAIVE:
		entry["password"] = account.Password
		// Optional Basic-auth username; empty means "use Email", which is what
		// every naive account created before the field existed relies on. Only
		// written when set, so those accounts' JSON does not grow a key.
		if account.NaiveUser != "" {
			entry["username"] = account.NaiveUser
		} else {
			delete(entry, "username")
		}
	case model.TUIC:
		// Authenticates with a uuid AND a password, and is keyed on the uuid.
		entry["id"] = account.UUID
		entry["password"] = account.Password
	case model.Hysteria, model.Hysteria2:
		entry["auth"] = account.Auth
	case model.L2TP, model.PPTP, model.OPENVPN, model.OPENCONNECT, model.SSTP, model.IKEV2:
		// Username AND password. The username lives in "id"; the password is the
		// identity these are addressed by (clientIdentityKey).
		entry["id"] = account.VpnUsername
		entry["password"] = account.Password
	case model.SSH:
		// SSH's "id" is a REAL login username, compared directly against what the
		// client offers (web/service/ssh.go:371). It is emphatically not the email,
		// despite two in-tree comments that say so. Rendering the email here would
		// change the login name of every existing SSH account on first projection.
		entry["id"] = account.VpnUsername
		entry["password"] = account.Password
	case model.MTPROTO:
		// Identity is the email; the secret is the credential.
		entry["id"] = account.Email
		entry["secret"] = account.Secret
	case model.WGC, model.AWG, model.GRE:
		// Nothing reads "id" for these three (verified: no .ID reference in
		// wgc.go/awg.go/gre.go); the email is the identity and the per-device
		// keypairs live in the passed-through part of the entry. Written anyway so
		// the JSON keeps the shape the rest of the panel expects.
		entry["id"] = account.Email
	default:
		entry["id"] = account.UUID
	}
}

// renderClientEntry produces the settings.clients entry for one membership,
// starting from whatever that entry already was.
func renderClientEntry(account *model.Account, membership *model.AccountInbound, protocol model.Protocol, existing map[string]any) map[string]any {
	entry := map[string]any{}
	// Prefer the live entry we are updating; fall back to the membership's stored
	// copy (which is what a re-created inbound or a fresh projection has).
	source := existing
	if source == nil && membership.Extra != "" {
		var stored map[string]any
		if err := json.Unmarshal([]byte(membership.Extra), &stored); err == nil {
			source = stored
		}
	}
	for k, v := range source {
		entry[k] = v
	}

	entry["email"] = account.Email
	entry["subId"] = account.SubID
	entry["totalGB"] = account.TotalGB
	entry["expiryTime"] = account.ExpiryTime
	entry["enable"] = account.Enable
	entry["reset"] = account.Reset
	entry["limitIp"] = account.LimitIP
	entry["tgId"] = account.TgID
	entry["comment"] = account.Comment

	applyAccountCredential(entry, account, protocol)

	// vless carries a per-membership flow override; every other protocol leaves
	// whatever the entry already had.
	if protocol == model.VLESS {
		if membership.Flow != "" {
			entry["flow"] = membership.Flow
		} else {
			delete(entry, "flow")
		}
	}

	// The slot is written EXPLICITLY for every pool protocol, always. That is what
	// makes removing another account from the array safe: without a stored slot,
	// each remaining account falls back to its list index, so a compaction
	// renumbers everyone after the hole and moves their tunnel addresses.
	if slotPoolProtocol(protocol) && membership.Slot != nil {
		entry["slot"] = *membership.Slot
	}

	return entry
}

// projectAccountOntoInbound splices one account's entry into one inbound's
// settings, in place, matched by email. It returns the new settings JSON.
//
// Adding a membership appends; the caller must have allocated the slot first.
func projectAccountOntoInbound(inbound *model.Inbound, account *model.Account, membership *model.AccountInbound) (string, error) {
	var root map[string]any
	if err := json.Unmarshal([]byte(inbound.Settings), &root); err != nil {
		return "", common.NewErrorf("inbound %d has unparseable settings: %v", inbound.Id, err)
	}
	if root == nil {
		root = map[string]any{}
	}
	list, _ := root["clients"].([]any)

	key := accountKey(account.Email)
	found := false
	for i, item := range list {
		existing, ok := item.(map[string]any)
		if !ok {
			continue
		}
		email, _ := existing["email"].(string)
		if accountKey(email) != key {
			continue
		}
		list[i] = renderClientEntry(account, membership, inbound.Protocol, existing)
		found = true
		break
	}
	if !found {
		list = append(list, renderClientEntry(account, membership, inbound.Protocol, nil))
	}

	root["clients"] = list
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// removeAccountFromInbound drops an account's entry from an inbound's settings.
//
// Matching is by EMAIL, the account's stable identity, and never by credential:
// a credential can be rotated between the read and the write, and an entry that
// failed to match would be left behind as a live account nobody is billed for.
func removeAccountFromInbound(inbound *model.Inbound, email string) (string, bool, error) {
	var root map[string]any
	if err := json.Unmarshal([]byte(inbound.Settings), &root); err != nil {
		return "", false, common.NewErrorf("inbound %d has unparseable settings: %v", inbound.Id, err)
	}
	if root == nil {
		return inbound.Settings, false, nil
	}
	list, _ := root["clients"].([]any)

	key := accountKey(email)
	kept := make([]any, 0, len(list))
	removed := false
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			kept = append(kept, item)
			continue
		}
		entryEmail, _ := entry["email"].(string)
		if accountKey(entryEmail) == key {
			removed = true
			continue
		}
		kept = append(kept, item)
	}
	if !removed {
		return inbound.Settings, false, nil
	}

	root["clients"] = kept
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", false, err
	}
	return string(out), true, nil
}

// ProjectAccount rewrites every member inbound's settings from the account, and
// removes the account from every inbound it is no longer a member of.
//
// Runs inside the caller's transaction so a partial fan-out cannot be committed:
// an account written to three inbounds and half-removed from a fourth is exactly
// the free-traffic state this layer exists to prevent.
func (s *AccountService) ProjectAccount(tx *gorm.DB, accountId int) ([]int, error) {
	var account model.Account
	if err := tx.Where("id = ?", accountId).First(&account).Error; err != nil {
		return nil, err
	}

	var memberships []model.AccountInbound
	if err := tx.Where("account_id = ?", accountId).Order("inbound_id ASC").Find(&memberships).Error; err != nil {
		return nil, err
	}

	member := make(map[int]*model.AccountInbound, len(memberships))
	for i := range memberships {
		member[memberships[i].InboundId] = &memberships[i]
	}

	// Every inbound that currently carries this email, so ex-memberships can be
	// cleaned up. A panel-wide scan rather than a diff against a remembered set:
	// the entry could have been put there by an older binary, by copyClients, or
	// by a DB import, and a leftover entry is a working account.
	var inbounds []*model.Inbound
	if err := tx.Model(&model.Inbound{}).Order("id ASC").Find(&inbounds).Error; err != nil {
		return nil, err
	}

	touched := make([]int, 0, len(memberships))
	for _, inbound := range inbounds {
		m, isMember := member[inbound.Id]
		if isMember {
			settings, err := projectAccountOntoInbound(inbound, &account, m)
			if err != nil {
				return nil, err
			}
			if settings == inbound.Settings {
				continue
			}
			if err := tx.Model(&model.Inbound{}).Where("id = ?", inbound.Id).
				Update("settings", settings).Error; err != nil {
				return nil, err
			}
			touched = append(touched, inbound.Id)
			continue
		}

		// Not a member. Only rewrite if the email is actually present, so this
		// stays a no-op for the overwhelming majority of inbounds.
		if !strings.Contains(strings.ToLower(inbound.Settings), accountKey(account.Email)) {
			continue
		}
		settings, removed, err := removeAccountFromInbound(inbound, account.Email)
		if err != nil {
			return nil, err
		}
		if !removed {
			continue
		}
		if err := tx.Model(&model.Inbound{}).Where("id = ?", inbound.Id).
			Update("settings", settings).Error; err != nil {
			return nil, err
		}
		touched = append(touched, inbound.Id)
	}

	return touched, nil
}
