package signature

import (
	"crypto/ecdsa"
	hex "encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1"
	hexeth "github.com/ethereum/go-ethereum/common/hexutil"
	crypto "github.com/ethereum/go-ethereum/crypto"
)

// AddressLengh is the lenght of an Ethereum address
const AddressLength = 20

// SignatureLength is the size of an ECDSA signature
const SignatureLength = 132

// PubKeyLength is the size of a Public Key
const PubKeyLength = 66

// SigningPrefix is the prefix added when hashing
const SigningPrefix = "\x19Ethereum Signed Message:\n"

// SignKeys represents an ECDSA pair of keys for signing.
// Authorized addresses is a list of Ethereum like addresses which are checked on Verify
type SignKeys struct {
	Public     *ecdsa.PublicKey
	Private    *ecdsa.PrivateKey
	Authorized []Address
}

// Address is an Ethereum like adrress
type Address [AddressLength]byte

// Generate generates new keys
func (k *SignKeys) Generate() error {
	var err error
	key, err := crypto.GenerateKey()
	if err != nil {
		return err
	}
	k.Public = &key.PublicKey
	k.Private = key
	return nil
}

// AddHexKey imports a private hex key
func (k *SignKeys) AddHexKey(privHex string) error {
	var err error
	k.Private, err = crypto.HexToECDSA(sanitizeHex(privHex))
	if err == nil {
		k.Public = &k.Private.PublicKey
	}
	return err
}

// AddAuthKey adds a new authorized address key
func (k *SignKeys) AddAuthKey(address string) error {
	if len(address) == AddressLength {
		var addr Address
		copy(addr[:], []byte(address)[:])
		k.Authorized = append(k.Authorized, addr)
		return nil
	} else {
		return errors.New("Invalid address lenght")
	}
}

// HexString returns the public and private keys as hex strings
func (k *SignKeys) HexString() (string, string) {
	pubHex := hex.EncodeToString(crypto.CompressPubkey(k.Public))
	privHex := hex.EncodeToString(crypto.FromECDSA(k.Private))
	return sanitizeHex(pubHex), sanitizeHex(privHex)
}

// EthAddrString return the Ethereum address from the Signing ECDSA public key
func (k *SignKeys) EthAddrString() string {
	recoveredAddr := [20]byte(crypto.PubkeyToAddress(*k.Public))
	return fmt.Sprintf("%x", recoveredAddr)
}

// Sign signs a message. Message is a normal string (no HexString nor a Hash)
func (k *SignKeys) Sign(message string) (string, error) {
	if k.Private == nil {
		return "", errors.New("No private key available")
	}
	hash := Hash(message)
	signature, err := crypto.Sign(hash, k.Private)
	if err != nil {
		return "", err
	}
	signHex := hex.EncodeToString(signature)
	return fmt.Sprintf("0x%s", signHex), err
}

// SignJSON signs a JSON message. Message is a struct interface
func (k *SignKeys) SignJSON(message interface{}) (string, error) {
	rawMsg, err := json.Marshal(message)
	if err != nil {
		return "", errors.New("unable to marshal message to sign: %s")
	}
	sig, err := k.Sign(string(rawMsg))
	if err != nil {
		return "", errors.New("error signing response body: %s")
	}
	return sig, nil
}

// Verify verifies a message. Signature and pubHex are HexStrings
func (k *SignKeys) Verify(message, signHex, pubHex string) (bool, error) {
	signature, err := hex.DecodeString(sanitizeHex(signHex))
	if err != nil {
		return false, err
	}
	pub, err := hex.DecodeString(sanitizeHex(pubHex))
	if err != nil {
		return false, err
	}
	hash := Hash(message)
	result := crypto.VerifySignature(pub, hash, signature[:64])
	return result, nil
}

// Standalone function for verify a message
func Verify(message, signHex, pubHex string) (bool, error) {
	sk := new(SignKeys)
	return sk.Verify(message, signHex, pubHex)
}

// Standaolone function to obtain the Ethereum address from a ECDSA public key
func AddrFromPublicKey(pubHex string) (string, error) {
	pubBytes, err := hex.DecodeString(pubHex)
	if err != nil {
		return "", err
	}
	pub, err := crypto.DecompressPubkey(pubBytes)
	//pub, err := crypto.UnmarshalPubkey(pubBytes)
	if err != nil {
		return "", err
	}
	recoveredAddr := [20]byte(crypto.PubkeyToAddress(*pub))
	return fmt.Sprintf("%x", recoveredAddr), nil
}

// ExtractEthAddr recovers the Ethereum address that created the signature of a message
func AddrFromSignature(msg, sigHex string) ([20]byte, error) {
	//sig := hexutil.MustDecode(sigHex)
	sig, err := hex.DecodeString(sanitizeHex(sigHex))
	if err != nil {
		return [20]byte{}, err
	}
	// Hack to avoid Hex codification problems with Ethereum
	sigHexEth := hexeth.Encode(sig)
	sig, err = hexeth.Decode(sigHexEth)
	if err != nil {
		return [20]byte{}, err
	}
	if sig[64] != 27 && sig[64] != 28 {
		return [20]byte{}, errors.New("Bad recovery hex")
	}
	sig[64] -= 27

	pubKey, err := crypto.SigToPub(Hash(msg), sig)
	if err != nil {
		return [20]byte{}, errors.New("Bad sig")
	}

	return [20]byte(crypto.PubkeyToAddress(*pubKey)), nil
}

// Hash string data adding Ethereum prefix
func Hash(data string) []byte {
	payloadToSign := fmt.Sprintf("%s%d%s", SigningPrefix, len(data), data)
	return crypto.Keccak256Hash([]byte(payloadToSign)).Bytes()
}

// HashRaw hashes a string with no prefix
func HashRaw(data string) []byte {
	return crypto.Keccak256Hash([]byte(data)).Bytes()
}

// VerifySender verifies if a message is sent by some Authorized address key
func (k *SignKeys) VerifySender(msg, sigHex string) (bool, error) {
	if len(k.Authorized) < 1 {
		return true, nil
	}
	recoveredAddr, err := AddrFromSignature(msg, sigHex)
	if err != nil {
		return false, err
	}
	for _, addr := range k.Authorized {
		if addr == recoveredAddr {
			return true, nil
		}
	}
	return false, nil
}

func sanitizeHex(hexStr string) string {
	if strings.HasPrefix(hexStr, "0x") {
		return fmt.Sprintf("%s", hexStr[2:])
	}
	return hexStr
}

// Encrypt uses secp256k1 standard from https://www.secg.org/sec2-v2.pdf to encrypt a message.
// The result is a Hexadecimal string
func (k *SignKeys) Encrypt(message string) (string, error) {
	pub, _ := k.HexString()
	pubBytes, err := hex.DecodeString(pub)
	if err != nil {
		return "", err
	}
	pubKey, err := secp256k1.ParsePubKey(pubBytes)
	if err != nil {
		return "", err
	}
	ciphertext, err := secp256k1.Encrypt(pubKey, []byte(message))
	if err != nil {
		return "", err
	}
	return sanitizeHex(hex.EncodeToString(ciphertext)), nil
}

// Decrypt uses secp256k1 standard to decrypt a Hexadecimal string message
// The result is plain text (no hex encoded)
func (k *SignKeys) Decrypt(hexMessage string) (string, error) {
	_, priv := k.HexString()
	pkBytes, err := hex.DecodeString(priv)
	if err != nil {
		return "", err
	}
	privKey, _ := secp256k1.PrivKeyFromBytes(pkBytes)
	cipertext, err := hex.DecodeString(sanitizeHex(hexMessage))
	if err != nil {
		return "", err
	}
	plaintext, err := secp256k1.Decrypt(privKey, cipertext)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}
