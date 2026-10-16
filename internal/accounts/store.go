package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"pricing-engine/internal/security"
)

// Redis layout (JSON values):
//
//	acct:m:<id>        account record
//	acct:email:<email> -> id               (unique email index)
//	acct:ids           set of account ids
//	acct:key:<pk_id>   API key record
//	acct:mkey:<id>     -> pk_id            (one active key per merchant)
//	acct:doc:<id>      encrypted KYC document bytes
//	acct:audit         list of audit events, newest first, capped
const (
	kAccount  = "acct:m:"
	kEmail    = "acct:email:"
	kIDs      = "acct:ids"
	kKey      = "acct:key:"
	kMerchKey = "acct:mkey:"
	kDoc      = "acct:doc:"
	kAudit    = "acct:audit"
	auditCap  = 1000
)

var ErrEmailTaken = errors.New("email already registered")

type Store struct{ c *redis.Client }

func NewStore(c *redis.Client) *Store { return &Store{c: c} }

func (s *Store) Create(ctx context.Context, a *Account) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	ok, err := s.c.SetNX(ctx, kEmail+a.Email, a.ID, 0).Result()
	if err != nil {
		return err
	}
	if !ok {
		return ErrEmailTaken
	}
	_, err = s.c.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, kAccount+a.ID, b, 0)
		p.SAdd(ctx, kIDs, a.ID)
		return nil
	})
	return err
}

func (s *Store) Put(ctx context.Context, a *Account) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return s.c.Set(ctx, kAccount+a.ID, b, 0).Err()
}

func (s *Store) Get(ctx context.Context, id string) (*Account, error) {
	b, err := s.c.Get(ctx, kAccount+id).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, security.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var a Account
	return &a, json.Unmarshal(b, &a)
}

func (s *Store) GetByEmail(ctx context.Context, email string) (*Account, error) {
	id, err := s.c.Get(ctx, kEmail+email).Result()
	if errors.Is(err, redis.Nil) {
		return nil, security.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

func (s *Store) List(ctx context.Context) ([]*Account, error) {
	ids, err := s.c.SMembers(ctx, kIDs).Result()
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = kAccount + id
	}
	vals, err := s.c.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := make([]*Account, 0, len(vals))
	for _, v := range vals {
		str, ok := v.(string)
		if !ok {
			continue
		}
		var a Account
		if json.Unmarshal([]byte(str), &a) == nil {
			out = append(out, &a)
		}
	}
	return out, nil
}

func (s *Store) PutKey(ctx context.Context, k *APIKey) error {
	b, err := json.Marshal(k)
	if err != nil {
		return err
	}
	_, err = s.c.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, kKey+k.ID, b, 0)
		p.Set(ctx, kMerchKey+k.MerchantID, k.ID, 0)
		return nil
	})
	return err
}

func (s *Store) GetKey(ctx context.Context, id string) (*APIKey, error) {
	b, err := s.c.Get(ctx, kKey+id).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, security.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var k APIKey
	return &k, json.Unmarshal(b, &k)
}

// KeyFor returns the merchant's active key, or nil.
func (s *Store) KeyFor(ctx context.Context, merchantID string) (*APIKey, error) {
	id, err := s.c.Get(ctx, kMerchKey+merchantID).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	k, err := s.GetKey(ctx, id)
	if errors.Is(err, security.ErrNotFound) {
		return nil, nil
	}
	return k, err
}

func (s *Store) DeleteKeyFor(ctx context.Context, merchantID string) error {
	id, err := s.c.Get(ctx, kMerchKey+merchantID).Result()
	if errors.Is(err, redis.Nil) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.c.Del(ctx, kKey+id, kMerchKey+merchantID).Err()
}

func (s *Store) TouchKey(ctx context.Context, k *APIKey, at time.Time) {
	k.LastUsedAt = at
	if b, err := json.Marshal(k); err == nil {
		s.c.Set(ctx, kKey+k.ID, b, 0)
	}
}

func (s *Store) PutDoc(ctx context.Context, id string, ct []byte) error {
	return s.c.Set(ctx, kDoc+id, ct, 0).Err()
}

func (s *Store) GetDoc(ctx context.Context, id string) ([]byte, error) {
	b, err := s.c.Get(ctx, kDoc+id).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, security.ErrNotFound
	}
	return b, err
}

func (s *Store) Audit(ctx context.Context, e AuditEvent) {
	b, _ := json.Marshal(e)
	s.c.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.LPush(ctx, kAudit, b)
		p.LTrim(ctx, kAudit, 0, auditCap-1)
		return nil
	})
}

// AuditPage returns events [offset, offset+n) newest first, plus the total.
func (s *Store) AuditPage(ctx context.Context, offset, n int64) ([]AuditEvent, int64, error) {
	var lr *redis.StringSliceCmd
	var ln *redis.IntCmd
	if _, err := s.c.Pipelined(ctx, func(p redis.Pipeliner) error {
		lr = p.LRange(ctx, kAudit, offset, offset+n-1)
		ln = p.LLen(ctx, kAudit)
		return nil
	}); err != nil {
		return nil, 0, err
	}
	ev, err := decodeAudit(lr.Val())
	return ev, ln.Val(), err
}

func (s *Store) AuditLog(ctx context.Context, n int64) ([]AuditEvent, error) {
	vals, err := s.c.LRange(ctx, kAudit, 0, n-1).Result()
	if err != nil {
		return nil, err
	}
	return decodeAudit(vals)
}

func decodeAudit(vals []string) ([]AuditEvent, error) {
	out := make([]AuditEvent, 0, len(vals))
	for _, v := range vals {
		var e AuditEvent
		if json.Unmarshal([]byte(v), &e) == nil {
			out = append(out, e)
		}
	}
	return out, nil
}
