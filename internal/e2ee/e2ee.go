// Package e2ee encrypts imported events. The importer owns one device on
// every account it sends as; each device encrypts with its own Megolm
// sessions, which are delivered to the real users who will read the history.
package e2ee

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/canonicaljson"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"slack2matrix/internal/config"
	"slack2matrix/internal/matrix"
	"slack2matrix/internal/store"
)

// Manager owns the per-account Olm machines.
type Manager struct {
	cfg   *config.Config
	mx    *matrix.Matrix
	store *store.Store
	log   zerolog.Logger
	db    *dbutil.Database

	// Recipients are the real users who receive every room key.
	Recipients []id.UserID

	mu       sync.Mutex
	machines map[id.UserID]*crypto.OlmMachine
	// sessionLock serializes session creation per room and sender.
	sessionLock sync.Map
}

// New opens the crypto database. recipients are the real users that must be
// able to decrypt the imported history.
func New(ctx context.Context, cfg *config.Config, mx *matrix.Matrix, st *store.Store, log zerolog.Logger, recipients []id.UserID) (*Manager, error) {
	if err := checkCanonicalJSON(); err != nil {
		return nil, err
	}
	if cfg.Crypto.PickleKey == "" {
		return nil, errors.New("crypto.pickle_key must be set")
	}
	sqlDB, err := store.OpenSQLite(filepath.Join(cfg.StateDir, "crypto.db"))
	if err != nil {
		return nil, err
	}
	db, err := dbutil.NewWithDB(sqlDB, "sqlite3")
	if err != nil {
		return nil, err
	}
	m := &Manager{
		cfg:        cfg,
		mx:         mx,
		store:      st,
		log:        log,
		db:         db,
		Recipients: recipients,
		machines:   map[id.UserID]*crypto.OlmMachine{},
	}
	// Upgrade the crypto schema once, via a throwaway store handle.
	probe := crypto.NewSQLCryptoStore(db, dbutil.ZeroLogger(log), "", "", []byte(cfg.Crypto.PickleKey))
	if err := probe.DB.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("upgrading crypto database: %w", err)
	}
	return m, nil
}

// Machine returns the Olm machine for an account, creating the importer's
// device and uploading its keys on first use.
func (m *Manager) Machine(ctx context.Context, user id.UserID) (*crypto.OlmMachine, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mach, ok := m.machines[user]; ok {
		return mach, nil
	}
	client := m.mx.Client(user)
	dev, _, err := m.store.Device(ctx, user)
	if err != nil {
		return nil, err
	}
	if dev == "" {
		dev = m.cfg.Crypto.DeviceID
		if err := client.CreateDeviceMSC4190(ctx, dev, m.cfg.Crypto.DeviceName); err != nil {
			return nil, fmt.Errorf("creating device for %s: %w", user, err)
		}
		if err := m.store.SetDevice(ctx, user, dev); err != nil {
			return nil, err
		}
	} else {
		client.DeviceID = dev
		client.SetAppServiceDeviceID = true
	}
	// mautrix logs expected first-pass misses (e.g. before claiming one-time
	// keys) as warnings; only surface real errors unless debugging.
	log := m.log.With().Stringer("account", user).Logger()
	if m.log.GetLevel() > zerolog.DebugLevel {
		log = log.Level(zerolog.ErrorLevel)
	}
	cs := cryptoStore{crypto.NewSQLCryptoStore(m.db, dbutil.ZeroLogger(log), string(user), dev, []byte(m.cfg.Crypto.PickleKey))}
	mach := crypto.NewOlmMachine(client, &log, cs, roomState{})
	// The importer never syncs, so it must not wait for key requests or
	// device list updates from other devices.
	mach.DisableDecryptKeyFetching = true
	if err := mach.Load(ctx); err != nil {
		return nil, fmt.Errorf("loading olm account for %s: %w", user, err)
	}
	if err := mach.ShareKeys(ctx, -1); err != nil {
		return nil, fmt.Errorf("uploading device keys for %s: %w", user, err)
	}
	if err := m.crossSign(ctx, mach, user); err != nil {
		return nil, err
	}
	m.machines[user] = mach
	return mach, nil
}

// Machines returns every loaded machine.
func (m *Manager) Machines() map[id.UserID]*crypto.OlmMachine {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[id.UserID]*crypto.OlmMachine, len(m.machines))
	for k, v := range m.machines {
		out[k] = v
	}
	return out
}

// LoadAllMachines loads a machine for every account with an importer device.
func (m *Manager) LoadAllMachines(ctx context.Context) error {
	devs, err := m.store.Devices(ctx)
	if err != nil {
		return err
	}
	for user := range devs {
		if _, err := m.Machine(ctx, user); err != nil {
			return err
		}
	}
	return nil
}

// ensureSession makes sure the sender has a current outbound Megolm session
// for the room covering the given year, creating and sharing one if needed.
func (m *Manager) ensureSession(ctx context.Context, mach *crypto.OlmMachine, room id.RoomID, sender id.UserID, year int) error {
	key := string(room) + "|" + string(sender)
	lockAny, _ := m.sessionLock.LoadOrStore(key, &sync.Mutex{})
	lock := lockAny.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	sid, sessYear, err := m.store.LatestMegolmSession(ctx, room, sender)
	if err != nil {
		return err
	}
	current, err := mach.CryptoStore.GetOutboundGroupSession(ctx, room)
	if err != nil {
		return err
	}
	if sid != "" && sessYear == year && current != nil && current.ID() == sid && current.Shared {
		return nil
	}
	if sid != "" && sessYear == year && current != nil && current.ID() == sid {
		// Created but sharing didn't finish (e.g. interrupted run).
		return m.share(ctx, mach, room)
	}
	if current != nil {
		if err := mach.CryptoStore.RemoveOutboundGroupSession(ctx, room); err != nil {
			return err
		}
	}
	ogs, err := crypto.NewOutboundGroupSession(room, nil, &event.HistoryVisibilityEventContent{HistoryVisibility: event.HistoryVisibilityShared})
	if err != nil {
		return err
	}
	// Rotation is driven by the importer (per year), not by message counts
	// or wall-clock age, which would otherwise expire sessions mid-import.
	ogs.MaxMessages = math.MaxInt32
	ogs.MaxAge = 100 * 365 * 24 * time.Hour
	signingKey, identityKey := mach.GetAccount().Keys()
	igs, err := crypto.NewInboundGroupSession(identityKey, signingKey, room, ogs.Internal.Key(), ogs.MaxAge, ogs.MaxMessages, ogs.SharedHistory, false)
	if err != nil {
		return err
	}
	igs.SourceUser = sender
	if err := mach.StoreGroupSession(ctx, igs); err != nil {
		return err
	}
	if err := mach.CryptoStore.AddOutboundGroupSession(ctx, ogs); err != nil {
		return err
	}
	if err := m.store.AddMegolmSession(ctx, room, sender, year, ogs.ID()); err != nil {
		return err
	}
	return m.share(ctx, mach, room)
}

// share sends the room's current outbound session to every recipient device.
func (m *Manager) share(ctx context.Context, mach *crypto.OlmMachine, room id.RoomID) error {
	// Refresh device lists so devices added since the last share are included.
	if _, err := mach.FetchKeys(ctx, m.Recipients, true); err != nil {
		return fmt.Errorf("fetching recipient device keys: %w", err)
	}
	if err := mach.ShareGroupSession(ctx, room, m.Recipients); err != nil {
		return fmt.Errorf("sharing megolm session: %w", err)
	}
	return nil
}

// Send encrypts and sends an event as sender with origin_server_ts=ts.
func (m *Manager) Send(ctx context.Context, room id.RoomID, sender id.UserID, evtType event.Type, content any, ts int64, txnID string) (id.EventID, error) {
	mach, err := m.Machine(ctx, sender)
	if err != nil {
		return "", err
	}
	year := time.UnixMilli(ts).UTC().Year()
	if err := m.ensureSession(ctx, mach, room, sender, year); err != nil {
		return "", err
	}
	enc, err := mach.EncryptMegolmEvent(ctx, room, evtType, content)
	if err != nil {
		return "", fmt.Errorf("encrypting: %w", err)
	}
	resp, err := mach.Client.SendMessageEvent(ctx, room, event.EventEncrypted, enc, mautrix.ReqSendEvent{
		Timestamp:     ts,
		TransactionID: txnID,
		DontEncrypt:   true,
	})
	if err != nil {
		return "", err
	}
	return resp.EventID, nil
}

// checkCanonicalJSON guards against a build where mautrix's canonical JSON
// signs one-time keys incorrectly (Go's json/v2 experiment calls the
// pointer-receiver MarshalJSON, signing the bare key string instead of the
// object). Keys signed that way are rejected by every other device.
func checkCanonicalJSON() error {
	data, err := canonicaljson.Marshal(mautrix.OneTimeKey{Key: "k"})
	if err != nil {
		return err
	}
	if string(data) != `{"key":"k"}` {
		return fmt.Errorf("this binary was built with encoding/json/v2, which breaks one-time key signatures; rebuild with GOEXPERIMENT=nojsonv2 (use `make build`)")
	}
	return nil
}

// cryptoStore fixes up outbound sessions loaded from SQL. mautrix normally
// shares sessions it has just created in memory; the importer creates them
// itself (to control rotation), and a reloaded session lacks the per-device
// map ShareGroupSession writes into.
type cryptoStore struct {
	*crypto.SQLCryptoStore
}

func (s cryptoStore) GetOutboundGroupSession(ctx context.Context, room id.RoomID) (*crypto.OutboundGroupSession, error) {
	ogs, err := s.SQLCryptoStore.GetOutboundGroupSession(ctx, room)
	if ogs != nil && ogs.Users == nil {
		ogs.Users = map[crypto.UserDevice]crypto.OGSState{}
	}
	return ogs, err
}

// roomState answers the crypto machine's questions about rooms. Every room
// the importer writes to is encrypted with shared history.
type roomState struct{}

func (roomState) IsEncrypted(context.Context, id.RoomID) (bool, error) { return true, nil }

func (roomState) GetEncryptionEvent(context.Context, id.RoomID) (*event.EncryptionEventContent, error) {
	return &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}, nil
}

func (roomState) GetHistoryVisibility(context.Context, id.RoomID) (*event.HistoryVisibilityEventContent, error) {
	return &event.HistoryVisibilityEventContent{HistoryVisibility: event.HistoryVisibilityShared}, nil
}

func (roomState) FindSharedRooms(context.Context, id.UserID) ([]id.RoomID, error) { return nil, nil }
