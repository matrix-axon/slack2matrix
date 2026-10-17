package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/crypto/signatures"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"slack2matrix/internal/config"
	"slack2matrix/internal/e2ee"
	"slack2matrix/internal/importer"
	"slack2matrix/internal/slackexport"
	"slack2matrix/internal/store"
)

// The test client plays the part of a real user's device: it logs in with a
// password (before the import, so it receives room keys over Olm), and
// afterwards syncs and decrypts the imported history to check it.

const testClientPickleKey = "slack2matrix-testclient"

func openTestClient(ctx context.Context, hsURL, user, password, dbPath string) (*mautrix.Client, *cryptohelper.CryptoHelper, error) {
	cli, err := mautrix.NewClient(hsURL, "", "")
	if err != nil {
		return nil, nil, err
	}
	helper, err := cryptohelper.NewCryptoHelper(cli, []byte(testClientPickleKey), dbPath)
	if err != nil {
		return nil, nil, err
	}
	localpart, _, _ := id.UserID(user).Parse()
	if localpart == "" {
		localpart = strings.TrimPrefix(user, "@")
	}
	helper.LoginAs = &mautrix.ReqLogin{
		Type:                     mautrix.AuthTypePassword,
		Identifier:               mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: localpart},
		Password:                 password,
		InitialDeviceDisplayName: "slack2matrix test client",
	}
	if err := helper.Init(ctx); err != nil {
		return nil, nil, err
	}
	cli.Crypto = helper
	return cli, helper, nil
}

// syncAll runs incremental syncs until the server has nothing more to send,
// so all queued to-device room keys get processed.
func syncAll(ctx context.Context, cli *mautrix.Client) error {
	since := ""
	if st, ok := cli.Store.(mautrix.SyncStore); ok {
		since, _ = st.LoadNextBatch(ctx, cli.UserID)
	}
	for i := 0; i < 1000; i++ {
		resp, err := cli.FullSyncRequest(ctx, mautrix.ReqSync{Since: since, Timeout: 0})
		if err != nil {
			return err
		}
		if err := cli.Syncer.ProcessResponse(ctx, resp, since); err != nil {
			return err
		}
		if st, ok := cli.Store.(mautrix.SyncStore); ok {
			_ = st.SaveNextBatch(ctx, cli.UserID, resp.NextBatch)
		}
		if len(resp.ToDevice.Events) == 0 && since != "" {
			return nil
		}
		since = resp.NextBatch
	}
	return errors.New("sync didn't settle")
}

type roomCheck struct {
	name                                string
	encrypted, decrypted, plaintextMsgs int
	failures                            map[string]int
	tsMismatch, withSource              int
	// expected is the importer's sent count for the room, or -1 if unknown.
	expected int
	byType   map[string]int
}

func runTestClient(ctx context.Context, cfg *config.Config, _ string, args []string) error {
	if len(args) < 1 || (args[0] != "login" && args[0] != "check") {
		return errors.New("usage: testclient login|check -user @u:server -password pw")
	}
	mode := args[0]
	fs := flag.NewFlagSet("testclient", flag.ExitOnError)
	user := fs.String("user", "", "Matrix user to log in as")
	password := fs.String("password", "", "password")
	dbPath := fs.String("db", "", "client crypto database (default: <state_dir>/testclient-<localpart>.db)")
	verbose := fs.Bool("v", false, "list each decryption failure")
	dump := fs.String("dump", "", "print decrypted events of the room with this name")
	setupRecovery := fs.String("setup-recovery", "", "login: set up cross-signing and key backup like Element, writing the recovery key to this file")
	restore := fs.String("restore-backup", "", "check: restore keys from the server-side backup using the recovery key in this file")
	importKeys := fs.String("import-keys", "", "check: import this key export file before checking")
	importPass := fs.String("import-passphrase-file", "", "check: passphrase file for -import-keys")
	_ = fs.Parse(args[1:])
	if *user == "" || *password == "" {
		return errors.New("-user and -password are required")
	}
	if *dbPath == "" {
		lp, _, _ := id.UserID(*user).Parse()
		*dbPath = filepath.Join(cfg.StateDir, "testclient-"+lp+".db")
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o700); err != nil {
		return err
	}
	cli, helper, err := openTestClient(ctx, cfg.Homeserver.URL, *user, *password, *dbPath)
	if err != nil {
		return err
	}
	defer helper.Close()
	mach := helper.Machine()
	if err := mach.ShareKeys(ctx, -1); err != nil {
		return fmt.Errorf("uploading device keys: %w", err)
	}
	if mode == "login" {
		if *setupRecovery != "" {
			rk, err := setUpRecovery(ctx, cli, helper, *password)
			if err != nil {
				return fmt.Errorf("setting up recovery: %w", err)
			}
			if err := os.WriteFile(*setupRecovery, []byte(rk+"\n"), 0o600); err != nil {
				return err
			}
			fmt.Printf("set up cross-signing and key backup; recovery key written to %s\n", *setupRecovery)
		}
		fmt.Printf("%s device %s is ready; it will receive room keys during the import\n", cli.UserID, cli.DeviceID)
		return nil
	}
	if *restore != "" {
		rk, err := e2ee.ReadSecretFile(*restore)
		if err != nil {
			return err
		}
		n, err := restoreBackup(ctx, helper, rk)
		if err != nil {
			return fmt.Errorf("restoring backup: %w", err)
		}
		fmt.Printf("restored backup (%d sessions in store)\n", n)
	}
	if *importKeys != "" {
		pass, err := e2ee.ReadSecretFile(*importPass)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(*importKeys)
		if err != nil {
			return err
		}
		imported, total, err := mach.ImportKeys(ctx, pass, data)
		if err != nil {
			return fmt.Errorf("importing keys: %w", err)
		}
		fmt.Printf("imported %d of %d sessions from %s\n", imported, total, *importKeys)
	}
	if err := syncAll(ctx, cli); err != nil {
		return fmt.Errorf("syncing: %w", err)
	}
	joined, err := cli.JoinedRooms(ctx)
	if err != nil {
		return err
	}
	// Compare against what the importer recorded, when its state is local.
	var expected map[id.RoomID]int
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "import.db")); err == nil {
		if st, err := store.Open(cfg.StateDir); err == nil {
			expected, _ = st.SentCounts(ctx)
			st.Close()
		}
	}
	var results []*roomCheck
	for _, room := range joined.JoinedRooms {
		var name event.RoomNameEventContent
		_ = cli.StateEvent(ctx, room, event.StateRoomName, "", &name)
		rc := &roomCheck{name: name.Name, failures: map[string]int{}, byType: map[string]int{}, expected: -1}
		if n, ok := expected[room]; ok {
			rc.expected = n
		}
		if rc.name == "" {
			rc.name = string(room)
		}
		from := ""
		for {
			resp, err := cli.Messages(ctx, room, from, "", mautrix.DirectionBackward, nil, 1000)
			if err != nil {
				return err
			}
			for _, evt := range resp.Chunk {
				dec := checkEvent(ctx, helper, evt, rc, *verbose)
				if dec != nil && *dump != "" && rc.name == *dump {
					out, _ := json.Marshal(map[string]any{"ts": dec.Timestamp, "sender": dec.Sender, "type": dec.Type.Type, "content": dec.Content.Raw})
					fmt.Println(string(out))
				}
			}
			if resp.End == "" || len(resp.Chunk) == 0 {
				break
			}
			from = resp.End
		}
		results = append(results, rc)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].name < results[j].name })
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "room\texpected\tencrypted\tdecrypted\tfailed\tplaintext msgs\tts mismatch\ttypes")
	var totalFail, totalPlain, totalTS, countMismatch int
	for _, rc := range results {
		failed := rc.encrypted - rc.decrypted
		totalFail += failed
		totalPlain += rc.plaintextMsgs
		totalTS += rc.tsMismatch
		var types []string
		for t, n := range rc.byType {
			types = append(types, fmt.Sprintf("%s=%d", t, n))
		}
		sort.Strings(types)
		exp := "?"
		if rc.expected >= 0 {
			exp = fmt.Sprint(rc.expected)
			if rc.expected != rc.withSource {
				countMismatch++
				exp += " (!)"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%s\n", rc.name, exp, rc.encrypted, rc.decrypted, failed, rc.plaintextMsgs, rc.tsMismatch, strings.Join(types, " "))
		for reason, n := range rc.failures {
			fmt.Fprintf(tw, "  failure\t%d× %s\n", n, reason)
		}
	}
	_ = tw.Flush()
	if totalFail > 0 || totalPlain > 0 || totalTS > 0 || countMismatch > 0 {
		return fmt.Errorf("verification failed: %d undecryptable, %d plaintext messages, %d timestamp mismatches, %d rooms whose event count differs from the import record",
			totalFail, totalPlain, totalTS, countMismatch)
	}
	fmt.Println("OK: every imported event decrypted, nothing was sent in plaintext, and timestamps match Slack")
	return nil
}

// setUpRecovery does what Element's "Set up recovery" does: secret storage,
// cross-signing keys, and a key backup whose key is kept in secret storage.
func setUpRecovery(ctx context.Context, cli *mautrix.Client, helper *cryptohelper.CryptoHelper, password string) (string, error) {
	mach := helper.Machine()
	key, err := mach.SSSS.GenerateAndUploadKey(ctx, "")
	if err != nil {
		return "", err
	}
	if err := mach.SSSS.SetDefaultKeyID(ctx, key.ID); err != nil {
		return "", err
	}
	keys, err := mach.GenerateCrossSigningKeys()
	if err != nil {
		return "", err
	}
	if err := mach.UploadCrossSigningKeysToSSSS(ctx, key, keys); err != nil {
		return "", err
	}
	err = mach.PublishCrossSigningKeys(ctx, keys, func(ui *mautrix.RespUserInteractive) any {
		return &mautrix.ReqUIAuthLogin{
			BaseAuthData: mautrix.BaseAuthData{Type: mautrix.AuthTypePassword, Session: ui.Session},
			User:         cli.UserID.String(),
			Password:     password,
		}
	})
	if err != nil {
		return "", err
	}
	if err := mach.SignOwnDevice(ctx, mach.OwnIdentity()); err != nil {
		return "", err
	}
	if err := mach.SignOwnMasterKey(ctx); err != nil {
		return "", err
	}
	bk, err := backup.NewMegolmBackupKey()
	if err != nil {
		return "", err
	}
	auth := backup.MegolmAuthData{PublicKey: id.Ed25519(base64.RawStdEncoding.EncodeToString(bk.PublicKey().Bytes()))}
	sig, err := keys.MasterKey.SignJSON(auth)
	if err != nil {
		return "", err
	}
	auth.Signatures = signatures.NewSingleSignature(cli.UserID, id.KeyAlgorithmEd25519, keys.MasterKey.PublicKey().String(), sig)
	if _, err := cli.CreateKeyBackupVersion(ctx, &mautrix.ReqRoomKeysVersionCreate[backup.MegolmAuthData]{
		Algorithm: id.KeyBackupAlgorithmMegolmBackupV1,
		AuthData:  auth,
	}); err != nil {
		return "", err
	}
	secret := base64.RawStdEncoding.EncodeToString(bk.Bytes())
	if err := mach.SSSS.SetEncryptedAccountData(ctx, event.AccountDataMegolmBackupKey, []byte(secret), key); err != nil {
		return "", err
	}
	return key.RecoveryKey(), nil
}

// restoreBackup downloads all keys from the backup, as a new device would.
func restoreBackup(ctx context.Context, helper *cryptohelper.CryptoHelper, recoveryKey string) (int, error) {
	mach := helper.Machine()
	keyID, keyData, err := mach.SSSS.GetDefaultKeyData(ctx)
	if err != nil {
		return 0, err
	}
	key, err := keyData.VerifyRecoveryKey(keyID, recoveryKey)
	if err != nil {
		return 0, err
	}
	secret, err := mach.SSSS.GetDecryptedAccountData(ctx, event.AccountDataMegolmBackupKey, key)
	if err != nil {
		return 0, err
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(string(secret), "="))
	if err != nil {
		return 0, err
	}
	bk, err := backup.MegolmBackupKeyFromBytes(raw)
	if err != nil {
		return 0, err
	}
	if _, err := mach.DownloadAndStoreLatestKeyBackup(ctx, bk); err != nil {
		return 0, err
	}
	n := 0
	iter := mach.CryptoStore.GetAllGroupSessions(ctx)
	err = iter.Iter(func(*crypto.InboundGroupSession) (bool, error) { n++; return true, nil })
	return n, err
}

// checkEvent tallies one event and returns its decrypted form, if any.
func checkEvent(ctx context.Context, helper *cryptohelper.CryptoHelper, evt *event.Event, rc *roomCheck, verbose bool) *event.Event {
	switch evt.Type {
	case event.EventEncrypted:
		rc.encrypted++
		if err := evt.Content.ParseRaw(evt.Type); err != nil {
			rc.failures["unparseable: "+err.Error()]++
			return nil
		}
		dec, err := helper.Decrypt(ctx, evt)
		if err != nil {
			reason := err.Error()
			if i := strings.Index(reason, ": "); i > 0 && !verbose {
				reason = reason[:i]
			}
			rc.failures[reason]++
			return nil
		}
		rc.decrypted++
		rc.byType[dec.Type.Type]++
		var src struct {
			Source *importer.Source `json:"net.slack2matrix.source"`
		}
		if raw, err := json.Marshal(dec.Content.Raw); err == nil {
			_ = json.Unmarshal(raw, &src)
		}
		if src.Source != nil {
			rc.withSource++
			if slackMs := slackexport.TS(src.Source.TS).Millis(); slackMs != 0 && slackMs != evt.Timestamp {
				// Placeholder thread roots are deliberately 1ms early.
				if evt.Timestamp != slackMs-1 {
					rc.tsMismatch++
				}
			}
		}
		return dec
	case event.EventMessage, event.EventReaction, event.EventSticker:
		rc.plaintextMsgs++
	}
	return nil
}
