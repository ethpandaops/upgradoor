package identity

import (
	"cmp"
	"crypto/hpke"
	"fmt"
	"slices"
	"sync"
	"time"
)

const (
	// SealKeyPeriod is how long an encryption key of the controller is its
	// current one.
	SealKeyPeriod = time.Hour
	// sealKeyLeadDivisor sets how early the next key is announced: a sixth
	// of the period (10 minutes of an hour).
	sealKeyLeadDivisor = 6
)

// Keyring holds the controller's encryption keys, in memory only. Key i is
// current from i periods after the Unix epoch for one period. The previous
// key stays accepted for one period after it was current, the next one is
// generated and announced a sixth of a period before it becomes current.
// A restarted controller knows none of its earlier keys: requests sealed to
// them get ErrUnknownSealKey.
type Keyring struct {
	now    func() time.Time
	period time.Duration

	mu   sync.Mutex
	keys []*ringKey // by index, at most previous, current and next
}

type ringKey struct {
	index int64
	priv  hpke.PrivateKey
	pub   *SealKey
}

// KeyringOption configures a Keyring.
type KeyringOption func(*Keyring)

// WithKeyringClock overrides the keyring's time source.
func WithKeyringClock(now func() time.Time) KeyringOption {
	return func(k *Keyring) { k.now = now }
}

// WithKeyPeriod overrides SealKeyPeriod.
func WithKeyPeriod(period time.Duration) KeyringOption {
	return func(k *Keyring) { k.period = period }
}

// NewKeyring returns an empty keyring; keys are generated when first needed.
func NewKeyring(opts ...KeyringOption) *Keyring {
	k := &Keyring{now: time.Now, period: SealKeyPeriod}

	for _, opt := range opts {
		opt(k)
	}

	return k
}

// Announced returns the current key and, from a sixth of a period before
// it becomes current, the next one.
func (k *Keyring) Announced() ([]*SealKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	cur, err := k.advance()
	if err != nil {
		return nil, err
	}

	out := make([]*SealKey, 0, 2)

	for _, rk := range k.keys {
		if rk.index >= cur {
			out = append(out, rk.pub)
		}
	}

	return out, nil
}

// UpToDate reports whether a sender that sealed to used and holds known
// has every announced key and uses the current one.
func (k *Keyring) UpToDate(used, known KeyID) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	cur, err := k.advance()
	if err != nil {
		return false, err
	}

	for _, rk := range k.keys {
		switch {
		case rk.index == cur && rk.pub.ID != used:
			return false, nil
		case rk.index > cur && rk.pub.ID != known:
			return false, nil
		}
	}

	return true, nil
}

// Open decrypts a sealed request under b and returns its body and the seal
// of its answer. A key the keyring does not hold is ErrUnknownSealKey,
// anything else that fails ErrSealed.
func (k *Keyring) Open(req *SealedRequest, b *SealBinding) ([]byte, *AnswerSeal, error) {
	priv, err := k.private(req.KeyID)
	if err != nil {
		return nil, nil, err
	}

	return openSealed(priv, req, b)
}

// private returns the private key of id. The decryption runs without the
// lock.
func (k *Keyring) private(id KeyID) (hpke.PrivateKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	if _, err := k.advance(); err != nil {
		return nil, err
	}

	for _, rk := range k.keys {
		if rk.pub.ID == id {
			return rk.priv, nil
		}
	}

	return nil, fmt.Errorf("%w: %s", ErrUnknownSealKey, id)
}

// advance drops the keys out of their window, generates the current and an
// announced next key, and returns the current index. k.mu is held.
func (k *Keyring) advance() (int64, error) {
	now := k.now()
	period := k.period.Nanoseconds()
	cur := now.UnixNano() / period

	k.keys = slices.DeleteFunc(k.keys, func(rk *ringKey) bool {
		return rk.index < cur-1 || rk.index > cur+1
	})

	if err := k.ensure(cur); err != nil {
		return 0, err
	}

	lead := k.period / sealKeyLeadDivisor
	if !now.Before(k.start(cur + 1).Add(-lead)) {
		if err := k.ensure(cur + 1); err != nil {
			return 0, err
		}
	}

	return cur, nil
}

// start is when key index becomes current.
func (k *Keyring) start(index int64) time.Time {
	return time.Unix(0, index*k.period.Nanoseconds()).UTC()
}

// ensure generates key index unless held. k.mu is held.
func (k *Keyring) ensure(index int64) error {
	if slices.ContainsFunc(k.keys, func(rk *ringKey) bool { return rk.index == index }) {
		return nil
	}

	kem, _, _ := sealSuite()

	priv, err := kem.GenerateKey()
	if err != nil {
		return fmt.Errorf("identity: generate encryption key: %w", err)
	}

	pub, err := NewSealKey(priv.PublicKey().Bytes(), k.start(index))
	if err != nil {
		return err
	}

	k.keys = append(k.keys, &ringKey{index: index, priv: priv, pub: pub})
	slices.SortFunc(k.keys, func(a, b *ringKey) int { return cmp.Compare(a.index, b.index) })

	return nil
}
