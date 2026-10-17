package importer

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"

	"slack2matrix/internal/e2ee"
)

// DeliverKeys uploads every imported session to the real users' key backups
// and writes the key export file.
func (imp *Importer) DeliverKeys(ctx context.Context, force bool) error {
	if err := imp.E2EE.LoadAllMachines(ctx); err != nil {
		return err
	}
	for _, user := range imp.Recipients() {
		res, err := imp.E2EE.UploadToBackup(ctx, user, force)
		if err != nil {
			return err
		}
		if res.Skipped != "" {
			imp.Log.Warn().Stringer("user", user).Str("reason", res.Skipped).Msg("Key backup skipped; this user can only read history on devices that existed during the import, or by importing the key file")
			continue
		}
		imp.Log.Info().Stringer("user", user).Str("version", string(res.Version)).Int("sessions", res.Uploaded).Msg("Uploaded keys to backup")
	}
	path := imp.Cfg.Crypto.KeyExportFile
	if path == "" {
		return nil
	}
	pass, err := imp.exportPassphrase()
	if err != nil {
		return err
	}
	n, err := imp.E2EE.ExportKeys(ctx, path, pass)
	if err != nil {
		return err
	}
	imp.Log.Info().Str("file", path).Int("sessions", n).Str("passphrase_file", imp.Cfg.Crypto.KeyExportPassphraseFile).Msg("Wrote key export")
	return nil
}

// exportPassphrase reads the key export passphrase, generating one if the
// passphrase file doesn't exist yet.
func (imp *Importer) exportPassphrase() (string, error) {
	path := imp.Cfg.Crypto.KeyExportPassphraseFile
	if path == "" {
		return "", fmt.Errorf("crypto.key_export_passphrase_file must be set to export keys")
	}
	if pass, err := e2ee.ReadSecretFile(path); err == nil && pass != "" {
		return pass, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	pass := base64.RawURLEncoding.EncodeToString(b)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return pass, os.WriteFile(path, []byte(pass+"\n"), 0o600)
}

// Finalize ends the import for the real users: notifications are turned
// back on and each room is marked read up to its last imported event.
func (imp *Importer) Finalize(ctx context.Context) error {
	for _, ch := range imp.Exp.Channels {
		room := imp.room(ch.ID)
		if room == "" {
			continue
		}
		last, err := imp.Store.LastEvent(ctx, ch.ID)
		if err != nil {
			return err
		}
		for _, user := range imp.Recipients() {
			if err := imp.Unmute(ctx, room, user); err != nil {
				return fmt.Errorf("unmuting %s for %s: %w", room, user, err)
			}
			if last == "" {
				continue
			}
			// Users who left the channel can't see its later events.
			if m, err := imp.Store.Membership(ctx, room, user); err != nil {
				return err
			} else if m != string(event.MembershipJoin) {
				continue
			}
			err := imp.MX.Client(user).SetReadMarkers(ctx, room, &mautrix.ReqSetReadMarkers{Read: last, FullyRead: last})
			if err != nil {
				return fmt.Errorf("setting read marker in %s for %s: %w", room, user, err)
			}
		}
		imp.Log.Info().Str("channel", ch.Name).Msg("Finalized")
	}
	return nil
}
