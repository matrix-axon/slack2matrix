package e2ee

import (
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/crypto/ssss"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// ReadSecretFile reads a recovery key or passphrase file, trimming whitespace.
func ReadSecretFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// recoveryKey returns a real user's configured recovery key, or "".
func (m *Manager) recoveryKey(user id.UserID) (string, error) {
	path := m.cfg.Crypto.RecoveryKeys[user]
	if path == "" {
		return "", nil
	}
	return ReadSecretFile(path)
}

// ssssKey unlocks a user's secret storage with their recovery key.
func ssssKey(ctx context.Context, mach *crypto.OlmMachine, recoveryKey string) (*ssss.Key, error) {
	keyID, keyData, err := mach.SSSS.GetDefaultKeyData(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading secret storage key: %w", err)
	}
	key, err := keyData.VerifyRecoveryKey(keyID, recoveryKey)
	if err != nil && !errors.Is(err, ssss.ErrUnverifiableKey) {
		return nil, fmt.Errorf("recovery key rejected: %w", err)
	}
	return key, nil
}

// CrossSign makes the importer's device trusted by its account's owner.
// Real users' devices are signed with the self-signing key from their secret
// storage (unlocked with the recovery key). Ghosts get a cross-signing
// identity of their own. The importer never creates or replaces a real
// user's cross-signing keys: Synapse lets appservices do that silently,
// which would reset the person's identity.
func (m *Manager) crossSign(ctx context.Context, mach *crypto.OlmMachine, user id.UserID) error {
	_, done, err := m.store.Device(ctx, user)
	if err != nil || done {
		return err
	}
	if slices.Contains(m.Recipients, user) {
		key, err := m.recoveryKey(user)
		if err != nil {
			return err
		}
		if key == "" {
			m.log.Warn().Stringer("user", user).Msg("No recovery key configured; the import device stays unverified until `slack2matrix crosssign` is run for this user")
			return nil
		}
		return m.signWithRecoveryKey(ctx, mach, user, key)
	} else {
		if existing, err := mach.GetOwnCrossSigningPublicKeys(ctx); err == nil && existing != nil {
			// Already has an identity (e.g. from an earlier run whose local
			// state was lost); leave it alone.
			m.log.Warn().Stringer("user", user).Msg("Ghost already has cross-signing keys that the importer doesn't hold; not re-signing")
			return m.store.SetCrossSigned(ctx, user)
		}
		keys, err := mach.GenerateCrossSigningKeys()
		if err != nil {
			return err
		}
		if err := mach.PublishCrossSigningKeys(ctx, keys, nil); err != nil {
			return fmt.Errorf("publishing cross-signing keys for %s: %w", user, err)
		}
		if err := mach.SignOwnDevice(ctx, mach.OwnIdentity()); err != nil {
			return fmt.Errorf("signing device of %s: %w", user, err)
		}
	}
	m.log.Info().Stringer("user", user).Msg("Import device cross-signed")
	return m.store.SetCrossSigned(ctx, user)
}

func (m *Manager) signWithRecoveryKey(ctx context.Context, mach *crypto.OlmMachine, user id.UserID, recoveryKey string) error {
	if err := mach.VerifyWithRecoveryKey(ctx, recoveryKey); err != nil {
		return fmt.Errorf("cross-signing import device of %s: %w", user, err)
	}
	m.log.Info().Stringer("user", user).Msg("Import device cross-signed")
	return m.store.SetCrossSigned(ctx, user)
}

// CrossSignWithRecoveryKey verifies the import device on a real user's
// account using a recovery key supplied at run time (it isn't stored). It
// can be run at any point, including long after the import: clients judge
// messages by the device's current verification state.
func (m *Manager) CrossSignWithRecoveryKey(ctx context.Context, user id.UserID, recoveryKey string) error {
	if !slices.Contains(m.Recipients, user) {
		return fmt.Errorf("%s is not one of the mapped real users", user)
	}
	if dev, _, err := m.store.Device(ctx, user); err != nil {
		return err
	} else if dev == "" {
		return fmt.Errorf("%s has no import device yet (nothing has been imported as this user)", user)
	}
	mach, err := m.Machine(ctx, user)
	if err != nil {
		return err
	}
	return m.signWithRecoveryKey(ctx, mach, user, recoveryKey)
}

// importedSessions loads every Megolm session the importer created.
func (m *Manager) importedSessions(ctx context.Context) ([]*crypto.InboundGroupSession, error) {
	refs, err := m.store.MegolmSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*crypto.InboundGroupSession, 0, len(refs))
	for _, ref := range refs {
		mach, err := m.Machine(ctx, ref.Sender)
		if err != nil {
			return nil, err
		}
		igs, err := mach.CryptoStore.GetGroupSession(ctx, ref.RoomID, ref.SessionID)
		if err != nil {
			return nil, err
		}
		if igs == nil {
			return nil, fmt.Errorf("session %s of %s in %s is missing from the crypto store", ref.SessionID, ref.Sender, ref.RoomID)
		}
		out = append(out, igs)
	}
	return out, nil
}

// ExportKeys writes every imported session to an Element-compatible,
// passphrase-protected key export file.
func (m *Manager) ExportKeys(ctx context.Context, path, passphrase string) (int, error) {
	sessions, err := m.importedSessions(ctx)
	if err != nil {
		return 0, err
	}
	data, err := crypto.ExportKeys(passphrase, sessions)
	if err != nil {
		return 0, err
	}
	return len(sessions), os.WriteFile(path, data, 0o600)
}

// BackupResult reports what UploadToBackup did for a user.
type BackupResult struct {
	Version  id.KeyBackupVersion
	Uploaded int
	Skipped  string
}

// UploadToBackup encrypts every imported session to a real user's
// server-side key backup, so any of their devices can restore the history.
// The backup must be trusted: either its key matches the one in the user's
// secret storage (needs the recovery key), or its auth data is signed by
// the user's cross-signing master key. force skips that check.
func (m *Manager) UploadToBackup(ctx context.Context, user id.UserID, force bool) (*BackupResult, error) {
	mach, err := m.Machine(ctx, user)
	if err != nil {
		return nil, err
	}
	var backupKey *backup.MegolmBackupKey
	if rk, err := m.recoveryKey(user); err != nil {
		return nil, err
	} else if rk != "" {
		if backupKey, err = backupKeyFromSSSS(ctx, mach, rk); err != nil {
			m.log.Warn().Err(err).Stringer("user", user).Msg("Couldn't read backup key from secret storage; falling back to signature check")
		}
	}
	info, err := mach.GetAndVerifyLatestKeyBackupVersion(ctx, backupKey)
	if err != nil {
		var httpErr mautrix.HTTPError
		if errors.As(err, &httpErr) && httpErr.RespError != nil && httpErr.RespError.ErrCode == "M_NOT_FOUND" {
			return &BackupResult{Skipped: "no key backup set up"}, nil
		}
		if !force {
			return nil, fmt.Errorf("%s's key backup isn't trusted (%w); pass -force to upload anyway", user, err)
		}
		if info, err = mach.Client.GetKeyBackupLatestVersion(ctx); err != nil {
			return nil, err
		}
	}
	pubBytes, err := base64.RawStdEncoding.DecodeString(string(info.AuthData.PublicKey))
	if err != nil {
		return nil, fmt.Errorf("bad backup public key: %w", err)
	}
	pub, err := ecdh.X25519().NewPublicKey(pubBytes)
	if err != nil {
		return nil, err
	}
	refs, err := m.store.MegolmSessions(ctx)
	if err != nil {
		return nil, err
	}
	method := "backup:" + string(info.Version)
	res := &BackupResult{Version: info.Version}
	req := &mautrix.ReqKeyBackup{Rooms: map[id.RoomID]mautrix.ReqRoomKeyBackup{}}
	pending := 0
	var pendingIDs []id.SessionID
	flush := func() error {
		if pending == 0 {
			return nil
		}
		if _, err := mach.Client.PutKeysInBackup(ctx, info.Version, req); err != nil {
			return fmt.Errorf("uploading to %s's backup: %w", user, err)
		}
		for _, sid := range pendingIDs {
			if err := m.store.SetKeyDelivered(ctx, sid, string(user), method); err != nil {
				return err
			}
		}
		res.Uploaded += pending
		req = &mautrix.ReqKeyBackup{Rooms: map[id.RoomID]mautrix.ReqRoomKeyBackup{}}
		pending, pendingIDs = 0, nil
		return nil
	}
	for _, ref := range refs {
		if done, err := m.store.KeyDelivered(ctx, ref.SessionID, string(user), method); err != nil {
			return nil, err
		} else if done {
			continue
		}
		senderMach, err := m.Machine(ctx, ref.Sender)
		if err != nil {
			return nil, err
		}
		igs, err := senderMach.CryptoStore.GetGroupSession(ctx, ref.RoomID, ref.SessionID)
		if err != nil || igs == nil {
			return nil, fmt.Errorf("loading session %s: %v", ref.SessionID, err)
		}
		first := igs.Internal.FirstKnownIndex()
		key, err := igs.Internal.Export(first)
		if err != nil {
			return nil, err
		}
		enc, err := backup.EncryptSessionDataWithPubkey(pub, backup.MegolmSessionData{
			Algorithm:          id.AlgorithmMegolmV1,
			ForwardingKeyChain: []string{},
			SenderClaimedKeys:  backup.SenderClaimedKeys{Ed25519: igs.SigningKey},
			SenderKey:          igs.SenderKey,
			SessionKey:         string(key),
			SharedHistory:      igs.SharedHistory,
		})
		if err != nil {
			return nil, err
		}
		encJSON, err := json.Marshal(enc)
		if err != nil {
			return nil, err
		}
		_, xsigned, _ := m.store.Device(ctx, ref.Sender)
		room := req.Rooms[ref.RoomID]
		if room.Sessions == nil {
			room.Sessions = map[id.SessionID]mautrix.ReqKeyBackupData{}
		}
		room.Sessions[ref.SessionID] = mautrix.ReqKeyBackupData{
			FirstMessageIndex: int(first),
			IsVerified:        xsigned,
			SessionData:       encJSON,
		}
		req.Rooms[ref.RoomID] = room
		pending++
		pendingIDs = append(pendingIDs, ref.SessionID)
		if pending >= 200 {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return res, nil
}

// backupKeyFromSSSS reads the user's backup private key from secret storage.
func backupKeyFromSSSS(ctx context.Context, mach *crypto.OlmMachine, recoveryKey string) (*backup.MegolmBackupKey, error) {
	key, err := ssssKey(ctx, mach, recoveryKey)
	if err != nil {
		return nil, err
	}
	data, err := mach.SSSS.GetDecryptedAccountData(ctx, event.AccountDataMegolmBackupKey, key)
	if err != nil {
		return nil, err
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(string(data), "="))
	if err != nil {
		return nil, fmt.Errorf("decoding backup key: %w", err)
	}
	return backup.MegolmBackupKeyFromBytes(raw)
}
