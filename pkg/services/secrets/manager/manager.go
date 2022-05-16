package manager

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/infra/usagestats"
	"github.com/grafana/grafana/pkg/services/encryption"
	"github.com/grafana/grafana/pkg/services/featuremgmt"
	"github.com/grafana/grafana/pkg/services/kmsproviders"
	"github.com/grafana/grafana/pkg/services/secrets"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/util"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"
	"xorm.io/xorm"
)

type SecretsService struct {
	store      secrets.Store
	enc        encryption.Internal
	settings   setting.Provider
	features   featuremgmt.FeatureToggles
	usageStats usagestats.Service

	mtx               sync.Mutex
	currentDataKey    *secrets.DataKey
	currentProviderID secrets.ProviderID

	providers    map[secrets.ProviderID]secrets.Provider
	dataKeyCache *dataKeyCache
	log          log.Logger
}

func ProvideSecretsService(
	store secrets.Store,
	kmsProvidersService kmsproviders.Service,
	enc encryption.Internal,
	settings setting.Provider,
	features featuremgmt.FeatureToggles,
	usageStats usagestats.Service,
) (*SecretsService, error) {
	providers, err := kmsProvidersService.Provide()
	if err != nil {
		return nil, err
	}

	logger := log.New("secrets")
	enabled := features.IsEnabled(featuremgmt.FlagEnvelopeEncryption)
	currentProviderID := kmsproviders.NormalizeProviderID(secrets.ProviderID(
		settings.KeyValue("security", "encryption_provider").MustString(kmsproviders.Default),
	))

	if _, ok := providers[currentProviderID]; enabled && !ok {
		return nil, fmt.Errorf("missing configuration for current encryption provider %s", currentProviderID)
	}

	if !enabled && currentProviderID != kmsproviders.Default {
		logger.Warn("Changing encryption provider requires enabling envelope encryption feature")
	}

	logger.Info("Envelope encryption state", "enabled", enabled, "current provider", currentProviderID)

	ttl := settings.KeyValue("security.encryption", "data_keys_cache_ttl").MustDuration(15 * time.Minute)
	cache := newDataKeyCache(ttl)

	s := &SecretsService{
		store:             store,
		enc:               enc,
		settings:          settings,
		usageStats:        usageStats,
		providers:         providers,
		currentProviderID: currentProviderID,
		dataKeyCache:      cache,
		features:          features,
		log:               logger,
	}

	s.registerUsageMetrics()

	return s, nil
}

func (s *SecretsService) registerUsageMetrics() {
	s.usageStats.RegisterMetricsFunc(func(context.Context) (map[string]interface{}, error) {
		usageMetrics := make(map[string]interface{})

		// Enabled / disabled
		usageMetrics["stats.encryption.envelope_encryption_enabled.count"] = 0
		if s.features.IsEnabled(featuremgmt.FlagEnvelopeEncryption) {
			usageMetrics["stats.encryption.envelope_encryption_enabled.count"] = 1
		}

		// Current provider
		kind, err := s.currentProviderID.Kind()
		if err != nil {
			return nil, err
		}
		usageMetrics[fmt.Sprintf("stats.encryption.current_provider.%s.count", kind)] = 1

		// Count by kind
		countByKind := make(map[string]int)
		for id := range s.providers {
			kind, err := id.Kind()
			if err != nil {
				return nil, err
			}

			countByKind[kind]++
		}

		for kind, count := range countByKind {
			usageMetrics[fmt.Sprintf(`stats.encryption.providers.%s.count`, kind)] = count
		}

		return usageMetrics, nil
	})
}

var b64 = base64.RawStdEncoding

func (s *SecretsService) Encrypt(ctx context.Context, payload []byte, opt secrets.EncryptionOptions) ([]byte, error) {
	return s.EncryptWithDBSession(ctx, payload, opt, nil)
}

func (s *SecretsService) EncryptWithDBSession(ctx context.Context, payload []byte, opt secrets.EncryptionOptions, sess *xorm.Session) ([]byte, error) {
	// Use legacy encryption service if envelopeEncryptionFeatureToggle toggle is off
	if !s.features.IsEnabled(featuremgmt.FlagEnvelopeEncryption) {
		return s.enc.Encrypt(ctx, payload, setting.SecretKey)
	}

	var err error
	defer func() {
		opsCounter.With(prometheus.Labels{
			"success":   strconv.FormatBool(err == nil),
			"operation": OpEncrypt,
		}).Inc()
	}()

	// If encryption featuremgmt.FlagEnvelopeEncryption toggle is on, use envelope encryption
	scope := opt()
	keyName := secrets.KeyName(scope, s.currentProviderID)

	var dataKey *secrets.DataKey

	s.mtx.Lock()
	if s.currentDataKey == nil {
		s.currentDataKey, err = s.getCurrentDataKey(ctx, keyName)
		if err != nil {
			if errors.Is(err, secrets.ErrDataKeyNotFound) {
				s.currentDataKey, err = s.newDataKey(ctx, keyName, scope, sess)
				s.mtx.Unlock()
				if err != nil {
					s.log.Error("Failed to generate new data key", "error", err, "name", keyName)
					return nil, err
				}
			} else {
				s.mtx.Unlock()
				s.log.Error("Failed to get current data key", "error", err, "name", keyName)
				return nil, err
			}
		}
	}
	dataKey = s.currentDataKey
	s.mtx.Unlock()

	var encrypted []byte
	encrypted, err = s.enc.Encrypt(ctx, payload, string(dataKey.DecryptedData))
	if err != nil {
		return nil, err
	}

	prefix := make([]byte, b64.EncodedLen(len(dataKey.Id))+2)
	b64.Encode(prefix[1:], []byte(dataKey.Id))
	prefix[0] = '#'
	prefix[len(prefix)-1] = '#'

	blob := make([]byte, len(prefix)+len(encrypted))
	copy(blob, prefix)
	copy(blob[len(prefix):], encrypted)

	return blob, nil
}

func (s *SecretsService) Decrypt(ctx context.Context, payload []byte) ([]byte, error) {
	// Use legacy encryption service if featuremgmt.FlagEnvelopeEncryption toggle is off
	if !s.features.IsEnabled(featuremgmt.FlagEnvelopeEncryption) {
		return s.enc.Decrypt(ctx, payload, setting.SecretKey)
	}

	// If encryption featuremgmt.FlagEnvelopeEncryption toggle is on, use envelope encryption
	var err error
	defer func() {
		opsCounter.With(prometheus.Labels{
			"success":   strconv.FormatBool(err == nil),
			"operation": OpDecrypt,
		}).Inc()
	}()

	if len(payload) == 0 {
		err = fmt.Errorf("unable to decrypt empty payload")
		return nil, err
	}

	var dataKey []byte

	if payload[0] != '#' {
		secretKey := s.settings.KeyValue("security", "secret_key").Value()
		dataKey = []byte(secretKey)
	} else {
		payload = payload[1:]
		endOfKey := bytes.Index(payload, []byte{'#'})
		if endOfKey == -1 {
			err = fmt.Errorf("could not find valid key id in encrypted payload")
			return nil, err
		}
		b64Key := payload[:endOfKey]
		payload = payload[endOfKey+1:]
		keyId := make([]byte, b64.DecodedLen(len(b64Key)))
		_, err = b64.Decode(keyId, b64Key)
		if err != nil {
			return nil, err
		}

		dataKey, err = s.dataKeyById(ctx, string(keyId))
		if err != nil {
			s.log.Error("Failed to lookup data key by id", "id", string(keyId), "error", err)
			return nil, err
		}
	}

	var decrypted []byte
	decrypted, err = s.enc.Decrypt(ctx, payload, string(dataKey))

	return decrypted, err
}

func (s *SecretsService) EncryptJsonData(ctx context.Context, kv map[string]string, opt secrets.EncryptionOptions) (map[string][]byte, error) {
	return s.EncryptJsonDataWithDBSession(ctx, kv, opt, nil)
}

func (s *SecretsService) EncryptJsonDataWithDBSession(ctx context.Context, kv map[string]string, opt secrets.EncryptionOptions, sess *xorm.Session) (map[string][]byte, error) {
	encrypted := make(map[string][]byte)
	for key, value := range kv {
		encryptedData, err := s.EncryptWithDBSession(ctx, []byte(value), opt, sess)
		if err != nil {
			return nil, err
		}

		encrypted[key] = encryptedData
	}
	return encrypted, nil
}

func (s *SecretsService) DecryptJsonData(ctx context.Context, sjd map[string][]byte) (map[string]string, error) {
	decrypted := make(map[string]string)
	for key, data := range sjd {
		decryptedData, err := s.Decrypt(ctx, data)
		if err != nil {
			return nil, err
		}

		decrypted[key] = string(decryptedData)
	}
	return decrypted, nil
}

func (s *SecretsService) GetDecryptedValue(ctx context.Context, sjd map[string][]byte, key, fallback string) string {
	if value, ok := sjd[key]; ok {
		decryptedData, err := s.Decrypt(ctx, value)
		if err != nil {
			return fallback
		}

		return string(decryptedData)
	}

	return fallback
}

func newRandomDataKey() ([]byte, error) {
	rawDataKey := make([]byte, 16)
	_, err := rand.Read(rawDataKey)
	if err != nil {
		return nil, err
	}
	return rawDataKey, nil
}

// newDataKey creates a new random data key, caches it and returns its value
func (s *SecretsService) newDataKey(ctx context.Context, name string, scope string, sess *xorm.Session) (*secrets.DataKey, error) {
	// 1. Create new data key
	dataKey, err := newRandomDataKey()
	if err != nil {
		return nil, err
	}
	provider, exists := s.providers[s.currentProviderID]
	if !exists {
		return nil, fmt.Errorf("could not find encryption provider '%s'", s.currentProviderID)
	}

	// 2. Encrypt it
	encrypted, err := provider.Encrypt(ctx, dataKey)
	if err != nil {
		return nil, err
	}

	// 3. Store its encrypted value in db
	dek := &secrets.DataKey{
		Id:            util.GenerateShortUID(),
		Active:        true,
		Name:          name,
		Provider:      s.currentProviderID,
		EncryptedData: encrypted,
		DecryptedData: dataKey,
		Scope:         scope,
	}

	if sess == nil {
		err = s.store.CreateDataKey(ctx, dek)
	} else {
		err = s.store.CreateDataKeyWithDBSession(ctx, dek, sess)
	}

	if err != nil {
		return nil, err
	}

	// 4. Cache its unencrypted value and return it
	s.dataKeyCache.add(dek.Id, dataKey)

	return dek, nil
}

// dataKeyByName looks up DEK in cache or database, and decrypts it
func (s *SecretsService) dataKeyById(ctx context.Context, id string) ([]byte, error) {
	if dataKey, exists := s.dataKeyCache.get(id); exists {
		return dataKey, nil
	}

	// 1. get encrypted data key from database
	dataKey, err := s.store.GetDataKey(ctx, id)
	if err != nil {
		return nil, err
	}

	// 2. decrypt data key
	provider, exists := s.providers[kmsproviders.NormalizeProviderID(dataKey.Provider)]
	if !exists {
		return nil, fmt.Errorf("could not find encryption provider '%s'", dataKey.Provider)
	}

	decrypted, err := provider.Decrypt(ctx, dataKey.EncryptedData)
	if err != nil {
		return nil, err
	}

	// 3. cache data key
	s.dataKeyCache.add(id, decrypted)

	return decrypted, nil
}

// currentDataKey looks up current data key in database, and decrypts it
func (s *SecretsService) getCurrentDataKey(ctx context.Context, name string) (*secrets.DataKey, error) {
	// 1. get encrypted data key from database
	dataKey, err := s.store.GetCurrentDataKey(ctx, name)
	if err != nil {
		return nil, err
	}

	// 2. decrypt data key
	provider, exists := s.providers[kmsproviders.NormalizeProviderID(dataKey.Provider)]
	if !exists {
		return nil, fmt.Errorf("could not find encryption provider '%s'", dataKey.Provider)
	}

	dataKey.DecryptedData, err = provider.Decrypt(ctx, dataKey.EncryptedData)
	if err != nil {
		return nil, err
	}

	// 3. cache data key
	s.dataKeyCache.add(dataKey.Id, dataKey.DecryptedData)

	return dataKey, nil
}

func (s *SecretsService) GetProviders() map[secrets.ProviderID]secrets.Provider {
	return s.providers
}

func (s *SecretsService) RotateDataKeys(ctx context.Context) error {
	// Currently, for a specific instance of time, there's only a single active
	// data key. However, in the future, we may have more than one (i.e. scopes).
	s.mtx.Lock()
	defer s.mtx.Unlock()

	err := s.store.DisableDataKeys(ctx)
	if err != nil {
		s.log.Error("Failed to disable active data keys while rotating data key", "error", err)
		return err
	}

	s.currentDataKey = nil

	return nil
}

func (s *SecretsService) ReEncryptDataKeys(ctx context.Context) error {
	err := s.store.ReEncryptDataKeys(ctx, s.providers, s.currentProviderID)
	if err != nil {
		s.log.Error("Failed to re-encrypt data keys", "error", err)
		return err
	}

	s.dataKeyCache.flush()

	return nil
}

func (s *SecretsService) Run(ctx context.Context) error {
	gc := time.NewTicker(
		s.settings.KeyValue("security.encryption", "data_keys_cache_cleanup_interval").
			MustDuration(time.Minute),
	)

	grp, gCtx := errgroup.WithContext(ctx)

	for _, p := range s.providers {
		if svc, ok := p.(secrets.BackgroundProvider); ok {
			grp.Go(func() error {
				return svc.Run(gCtx)
			})
		}
	}

	for {
		select {
		case <-gc.C:
			s.log.Debug("removing expired data encryption keys from cache...")
			s.dataKeyCache.removeExpired()
			s.log.Debug("done removing expired data encryption keys from cache")
		case <-gCtx.Done():
			s.log.Debug("grafana is shutting down; stopping...")
			gc.Stop()

			if err := grp.Wait(); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}

			return nil
		}
	}
}
