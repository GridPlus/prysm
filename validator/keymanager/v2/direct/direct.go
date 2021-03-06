package direct

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"strconv"
	"sync"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	ethpb "github.com/prysmaticlabs/ethereumapis/eth/v1alpha1"
	"github.com/prysmaticlabs/go-ssz"
	contract "github.com/prysmaticlabs/prysm/contracts/deposit-contract"
	validatorpb "github.com/prysmaticlabs/prysm/proto/validator/accounts/v2"
	"github.com/prysmaticlabs/prysm/shared/bls"
	"github.com/prysmaticlabs/prysm/shared/bytesutil"
	"github.com/prysmaticlabs/prysm/shared/depositutil"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/prysmaticlabs/prysm/shared/roughtime"
	"github.com/sirupsen/logrus"
	keystorev4 "github.com/wealdtech/go-eth2-wallet-encryptor-keystorev4"
)

var log = logrus.WithField("prefix", "direct-keymanager-v2")

const (
	// DepositTransactionFileName for the encoded, eth1 raw deposit tx data
	// for a validator account.
	DepositTransactionFileName = "deposit_transaction.rlp"
	// TimestampFileName stores a timestamp for account creation as a
	// file for a direct keymanager account.
	TimestampFileName = "created_at.txt"
	// KeystoreFileName exposes the expected filename for the keystore file for an account.
	KeystoreFileName    = "keystore.json"
	depositDataFileName = "deposit_data.ssz"
	eipVersion          = "EIP-2335"
)

// Wallet defines a struct which has capabilities and knowledge of how
// to read and write important accounts-related files to the filesystem.
// Useful for keymanager to have persistent capabilities for accounts on-disk.
type Wallet interface {
	AccountsDir() string
	CanUnlockAccounts() bool
	AccountNames() ([]string, error)
	ReadPasswordForAccount(accountName string) (string, error)
	ReadFileForAccount(accountName string, fileName string) ([]byte, error)
	WriteAccountToDisk(ctx context.Context, password string) (string, error)
	WriteFileForAccount(ctx context.Context, accountName string, fileName string, data []byte) error
}

// Config for a direct keymanager.
type Config struct {
	EIPVersion string `json:"direct_eip_version"`
}

// Keymanager implementation for direct keystores utilizing EIP-2335.
type Keymanager struct {
	wallet            Wallet
	cfg               *Config
	mnemonicGenerator SeedPhraseFactory
	keysCache         map[[48]byte]bls.SecretKey
	lock              sync.RWMutex
}

// Keystore json file representation as a Go struct.
type Keystore struct {
	Crypto  map[string]interface{} `json:"crypto"`
	ID      string                 `json:"uuid"`
	Pubkey  string                 `json:"pubkey"`
	Version uint                   `json:"version"`
	Name    string                 `json:"name"`
}

// DefaultConfig for a direct keymanager implementation.
func DefaultConfig() *Config {
	return &Config{
		EIPVersion: eipVersion,
	}
}

// NewKeymanager instantiates a new direct keymanager from configuration options.
func NewKeymanager(ctx context.Context, wallet Wallet, cfg *Config, skipMnemonicConfirm bool) (*Keymanager, error) {
	k := &Keymanager{
		wallet: wallet,
		cfg:    cfg,
		mnemonicGenerator: &EnglishMnemonicGenerator{
			skipMnemonicConfirm: skipMnemonicConfirm,
		},
		keysCache: make(map[[48]byte]bls.SecretKey),
	}
	// If the wallet has the capability of unlocking accounts using
	// passphrases, then we initialize a cache of public key -> secret keys
	// used to retrieve secrets keys for the accounts via password unlock.
	// This cache is needed to process Sign requests using a public key.
	if wallet.CanUnlockAccounts() {
		if err := k.initializeSecretKeysCache(); err != nil {
			return nil, errors.Wrap(err, "could not initialize keys cache")
		}
	}
	return k, nil
}

// UnmarshalConfigFile attempts to JSON unmarshal a direct keymanager
// configuration file into the *Config{} struct.
func UnmarshalConfigFile(r io.ReadCloser) (*Config, error) {
	enc, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := r.Close(); err != nil {
			log.Errorf("Could not close keymanager config file: %v", err)
		}
	}()
	cfg := &Config{}
	if err := json.Unmarshal(enc, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// MarshalConfigFile returns a marshaled configuration file for a keymanager.
func MarshalConfigFile(ctx context.Context, cfg *Config) ([]byte, error) {
	return json.MarshalIndent(cfg, "", "\t")
}

// CreateAccount for a direct keymanager implementation. This utilizes
// the EIP-2335 keystore standard for BLS12-381 keystores. It
// stores the generated keystore.json file in the wallet and additionally
// generates a mnemonic for withdrawal credentials. At the end, it logs
// the raw deposit data hex string for users to copy.
func (dr *Keymanager) CreateAccount(ctx context.Context, password string) (string, error) {
	// Create a new, unique account name and write its password + directory to disk.
	accountName, err := dr.wallet.WriteAccountToDisk(ctx, password)
	if err != nil {
		return "", errors.Wrap(err, "could not write account to disk")
	}
	// Generates a new EIP-2335 compliant keystore file
	// from a BLS private key and marshals it as JSON.
	validatingKey := bls.RandKey()
	encoded, err := dr.generateKeystoreFile(validatingKey, password)
	if err != nil {
		return "", err
	}

	// Generate a withdrawal key and confirm user
	// acknowledgement of a 256-bit entropy mnemonic phrase.
	withdrawalKey := bls.RandKey()
	rawWithdrawalKey := withdrawalKey.Marshal()[:]
	seedPhrase, err := dr.mnemonicGenerator.Generate(rawWithdrawalKey)
	if err != nil {
		return "", errors.Wrap(err, "could not generate mnemonic for withdrawal key")
	}
	if err := dr.mnemonicGenerator.ConfirmAcknowledgement(seedPhrase); err != nil {
		return "", errors.Wrap(err, "could not confirm acknowledgement of mnemonic")
	}

	// Upon confirmation of the withdrawal key, proceed to display
	// and write associated deposit data to disk.
	tx, depositData, err := generateDepositTransaction(validatingKey, withdrawalKey)
	if err != nil {
		return "", errors.Wrap(err, "could not generate deposit transaction data")
	}

	// Log the deposit transaction data to the user.
	logDepositTransaction(tx)

	// We write the raw deposit transaction as an .rlp encoded file.
	if err := dr.wallet.WriteFileForAccount(ctx, accountName, DepositTransactionFileName, tx.Data()); err != nil {
		return "", errors.Wrapf(err, "could not write for account %s: %s", accountName, DepositTransactionFileName)
	}

	// We write the ssz-encoded deposit data to disk as a .ssz file.
	encodedDepositData, err := ssz.Marshal(depositData)
	if err != nil {
		return "", errors.Wrap(err, "could not marshal deposit data")
	}
	if err := dr.wallet.WriteFileForAccount(ctx, accountName, depositDataFileName, encodedDepositData); err != nil {
		return "", errors.Wrapf(err, "could not write for account %s: %s", accountName, encodedDepositData)
	}

	// Write the encoded keystore to disk.
	if err := dr.wallet.WriteFileForAccount(ctx, accountName, KeystoreFileName, encoded); err != nil {
		return "", errors.Wrapf(err, "could not write keystore file for account %s", accountName)
	}

	// Finally, write the account creation timestamp as a file.
	createdAt := roughtime.Now().Unix()
	createdAtStr := strconv.FormatInt(createdAt, 10)
	if err := dr.wallet.WriteFileForAccount(ctx, accountName, TimestampFileName, []byte(createdAtStr)); err != nil {
		return "", errors.Wrapf(err, "could not write timestamp file for account %s", accountName)
	}

	log.WithFields(logrus.Fields{
		"name": accountName,
		"path": dr.wallet.AccountsDir(),
	}).Info("Successfully created new validator account")
	return accountName, nil
}

// FetchValidatingPublicKeys fetches the list of public keys from the direct account keystores.
func (dr *Keymanager) FetchValidatingPublicKeys(ctx context.Context) ([][48]byte, error) {
	accountNames, err := dr.wallet.AccountNames()
	if err != nil {
		return nil, err
	}

	// Return the public keys from the cache if they match the
	// number of accounts from the wallet.
	publicKeys := make([][48]byte, len(accountNames))
	dr.lock.Lock()
	defer dr.lock.Unlock()
	if dr.keysCache != nil && len(dr.keysCache) == len(accountNames) {
		var i int
		for k := range dr.keysCache {
			publicKeys[i] = k
			i++
		}
		return publicKeys, nil
	}

	for i, name := range accountNames {
		encoded, err := dr.wallet.ReadFileForAccount(name, KeystoreFileName)
		if err != nil {
			return nil, errors.Wrapf(err, "could not read keystore file for account %s", name)
		}
		keystoreFile := &Keystore{}
		if err := json.Unmarshal(encoded, keystoreFile); err != nil {
			return nil, errors.Wrapf(err, "could not decode keystore json for account: %s", name)
		}
		pubKeyBytes, err := hex.DecodeString(keystoreFile.Pubkey)
		if err != nil {
			return nil, errors.Wrapf(err, "could not decode pubkey bytes: %#x", keystoreFile.Pubkey)
		}
		publicKeys[i] = bytesutil.ToBytes48(pubKeyBytes)
	}
	return publicKeys, nil
}

// Sign signs a message using a validator key.
func (dr *Keymanager) Sign(ctx context.Context, req *validatorpb.SignRequest) (bls.Signature, error) {
	rawPubKey := req.PublicKey
	if rawPubKey == nil {
		return nil, errors.New("nil public key in request")
	}
	dr.lock.RLock()
	defer dr.lock.RUnlock()
	secretKey, ok := dr.keysCache[bytesutil.ToBytes48(rawPubKey)]
	if !ok {
		return nil, errors.New("no signing key found in keys cache")
	}
	return secretKey.Sign(req.SigningRoot), nil
}

func (dr *Keymanager) initializeSecretKeysCache() error {
	accountNames, err := dr.wallet.AccountNames()
	if err != nil {
		return err
	}

	for _, name := range accountNames {
		password, err := dr.wallet.ReadPasswordForAccount(name)
		if err != nil {
			return errors.Wrapf(err, "could not read password for account %s", name)
		}
		encoded, err := dr.wallet.ReadFileForAccount(name, KeystoreFileName)
		if err != nil {
			return errors.Wrapf(err, "could not read keystore file for account %s", name)
		}
		keystoreFile := &Keystore{}
		if err := json.Unmarshal(encoded, keystoreFile); err != nil {
			return errors.Wrapf(err, "could not decode keystore json for account: %s", name)
		}
		// We extract the validator signing private key from the keystore
		// by utilizing the password and initialize a new BLS secret key from
		// its raw bytes.
		decryptor := keystorev4.New()
		rawSigningKey, err := decryptor.Decrypt(keystoreFile.Crypto, []byte(password))
		if err != nil {
			return errors.Wrapf(err, "could not decrypt validator signing key for account: %s", name)
		}
		validatorSigningKey, err := bls.SecretKeyFromBytes(rawSigningKey)
		if err != nil {
			return errors.Wrapf(err, "could not instantiate bls secret key from bytes for account: %s", name)
		}

		// Update a simple cache of public key -> secret key utilized
		// for fast signing access in the direct keymanager.
		dr.keysCache[bytesutil.ToBytes48(validatorSigningKey.PublicKey().Marshal())] = validatorSigningKey
	}
	return nil
}

func (dr *Keymanager) generateKeystoreFile(validatingKey bls.SecretKey, password string) ([]byte, error) {
	encryptor := keystorev4.New()
	cryptoFields, err := encryptor.Encrypt(validatingKey.Marshal(), []byte(password))
	if err != nil {
		return nil, errors.Wrap(err, "could not encrypt validating key into keystore")
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	keystoreFile := &Keystore{}
	keystoreFile.Crypto = cryptoFields
	keystoreFile.ID = id.String()
	keystoreFile.Pubkey = fmt.Sprintf("%x", validatingKey.PublicKey().Marshal())
	keystoreFile.Version = encryptor.Version()
	keystoreFile.Name = encryptor.Name()
	return json.MarshalIndent(keystoreFile, "", "\t")
}

func generateDepositTransaction(
	validatingKey bls.SecretKey,
	withdrawalKey bls.SecretKey,
) (*types.Transaction, *ethpb.Deposit_Data, error) {
	depositData, depositRoot, err := depositutil.DepositInput(
		validatingKey, withdrawalKey, params.BeaconConfig().MaxEffectiveBalance,
	)
	if err != nil {
		return nil, nil, errors.Wrap(err, "could not generate deposit input")
	}
	testAcc, err := contract.Setup()
	if err != nil {
		return nil, nil, errors.Wrap(err, "could not load deposit contract")
	}
	testAcc.TxOpts.GasLimit = 1000000

	tx, err := testAcc.Contract.Deposit(
		testAcc.TxOpts,
		depositData.PublicKey,
		depositData.WithdrawalCredentials,
		depositData.Signature,
		depositRoot,
	)
	return tx, depositData, nil
}

func logDepositTransaction(tx *types.Transaction) {
	log.Info(
		"Copy + paste the deposit data below when using the " +
			"eth1 deposit contract")
	fmt.Printf(`
========================Deposit Data===============================

%#x

===================================================================`, tx.Data())
	fmt.Printf(`
***Enter the above deposit data into step 3 on https://prylabs.net/participate***
`)
}
