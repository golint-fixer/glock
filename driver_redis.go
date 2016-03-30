package glock

import (
	"time"

	"github.com/garyburd/redigo/redis"
	"github.com/gocql/gocql"
)

const (
	releaseScriptText = `
if redis.call("get", KEYS[1]) == ARGV[1] then
  redis.call("del", KEYS[1])
	redis.call("del", KEYS[2])
	return 1
end
return 0
`
	refreshScriptText = `
if redis.call("get", KEYS[1]) == ARGV[1] then
  redis.call("set", KEYS[1], ARGV[1], "PX", ARGV[2])
	redis.call("set", KEYS[2], ARGV[3])
	return 1
end
return 0
`
	defaultNS = "glock"
)

var (
	releaseScript = redis.NewScript(2, releaseScriptText)
	refreshScript = redis.NewScript(2, refreshScriptText)
)

// DialFunc is a function prototype that matches redigo/redis.Dial signature.
type DialFunc func(network, address string, options ...redis.DialOption) (redis.Conn, error)

// RedisOptions represent options to connect to redis
type RedisOptions struct {
	// Network, i.e. 'tcp'
	Network string
	// Address, i.e. 'localhost:6379'
	Address string
	// ClientID is the current client ID. If not set, it will be autogenerated
	ClientID string
	// Namespace is an optional namespace for all redis keys that will be created.
	Namespace string
	// A list of redigo/redis.DialOption to be used when connecting to redis
	DialOptions []redis.DialOption
	// The function used to connect to redis. defaults to redigo/redis.Dial
	DialFunc DialFunc
}

// RedisClient implements the Client interface to manage locks in redis
type RedisClient struct {
	conn redis.Conn
	opts RedisOptions
}

// RedisLock implements the Lock interface for locks in the redis store
type RedisLock struct {
	name   string
	ttl    time.Duration
	client *RedisClient
	data   string
}

// NewRedisClient return a new RedisClient given the provided RedisOptions
func NewRedisClient(opts RedisOptions) (*RedisClient, error) {
	if opts.ClientID == "" {
		id, err := gocql.RandomUUID()
		if err != nil {
			return nil, err
		}
		opts.ClientID = id.String()
	}
	if opts.Network == "" {
		opts.Network = "tcp"
	}

	if opts.Namespace == "" {
		opts.Namespace = defaultNS
	}

	if opts.DialFunc == nil {
		opts.DialFunc = redis.Dial
	}
	c := RedisClient{nil, opts}
	err := c.Reconnect()
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Clone returns a disconnected copy of the currenct client
func (c *RedisClient) Clone() Client {
	return &RedisClient{
		opts: c.opts,
		conn: nil,
	}
}

// Close closes the connecton to redis
func (c *RedisClient) Close() {
	if c.conn != nil {
		c.conn.Close()
	}
}

// Reconnect reconnects to redis, or connects if not connected
func (c *RedisClient) Reconnect() error {
	c.Close()
	conn, err := c.opts.DialFunc(c.opts.Network, c.opts.Address, c.opts.DialOptions...)
	if err != nil {
		return err
	}
	c.conn = conn
	_, err = c.conn.Do("PING")
	if err != nil {
		return err
	}
	return nil
}

// SetID sets the ID for the current client
func (c *RedisClient) SetID(id string) {
	c.opts.ClientID = id
}

// ID returns the current client ID
func (c *RedisClient) ID() string {
	return c.opts.ClientID
}

func (l *RedisLock) key() string {
	return l.client.opts.Namespace + ":" + l.name
}

func (l *RedisLock) dataKey() string {
	return l.key() + ":data"
}

// NewLock creates a new Lock. Lock is not automatically acquired.
func (c *RedisClient) NewLock(name string) Lock {
	return &RedisLock{
		name:   name,
		ttl:    time.Duration(0),
		client: c,
	}
}

// Acquire acquires the lock for the specified time lentgh (ttl).
// It returns immadiately if the lock cannot be acquired
func (l *RedisLock) Acquire(ttl time.Duration) error {
	if ttl < time.Millisecond {
		return ErrInvalidTTL
	}
	l.ttl = ttl
	ms := int(ttl.Nanoseconds() / int64(time.Millisecond))
	_, err := redis.String(l.client.conn.Do("SET", l.key(), l.client.ID(), "PX", ms, "NX"))
	switch {
	case err == redis.ErrNil:
		return ErrLockHeldByOtherClient
	case err != nil:
		return err
	}
	l.client.conn.Do("SET", l.dataKey(), l.data)

	return nil
}

// Release releases the lock if owned. Returns an error if the lock is not owned by this client
func (l *RedisLock) Release() error {
	res, err := redis.Bool(releaseScript.Do(l.client.conn, l.key(), l.dataKey(), l.client.ID()))
	if err != nil {
		return err
	}
	if res == false {
		return ErrLockHeldByOtherClient
	}
	return nil
}

// RefreshTTL Extends the lock, if owned, for the specified TTL.
// ttl argument becomes the new ttl for the lock: successive calls to Refresh()
// will use this ttl
// It returns an error if the lock is not owned by the current client
func (l *RedisLock) RefreshTTL(ttl time.Duration) error {
	l.ttl = ttl
	return l.Refresh()
}

// Refresh extends the lock by extending the TTL in the store.
// It returns an error if the lock is not owned by the current client
func (l *RedisLock) Refresh() error {
	if l.ttl < time.Millisecond {
		return ErrInvalidTTL
	}
	ms := int(l.ttl.Nanoseconds() / int64(time.Millisecond))
	res, err := redis.Bool(refreshScript.Do(l.client.conn, l.key(), l.dataKey(), l.client.ID(), ms, l.data))
	if err != nil {
		return err
	}
	if res == false {
		return ErrLockNotOwned
	}
	return nil
}

// Info returns information about the lock.
func (l *RedisLock) Info() (*LockInfo, error) {
	var owner, data string
	var expire int

	l.client.conn.Send("MULTI")
	l.client.conn.Send("GET", l.key())
	l.client.conn.Send("PTTL", l.key())
	l.client.conn.Send("GET", l.dataKey())
	reply, err := redis.Values(l.client.conn.Do("EXEC"))

	if err == redis.ErrNil {
		return &LockInfo{l.name, false, "", time.Duration(0), ""}, nil
	}
	if err != nil {
		return nil, err
	}

	_, err = redis.Scan(reply, &owner, &expire, &data)
	if err != nil {
		return nil, err
	}

	ttl := time.Duration(expire) * time.Millisecond

	return &LockInfo{
		Name:     l.name,
		Acquired: ttl > 0,
		Owner:    owner,
		TTL:      ttl,
		Data:     data,
	}, nil
}

// SetData sets the data payload for the lock.
// The data is set into the backend only when the lock is acquired,
// so any call to this method after acquisition won't update the value.
func (l *RedisLock) SetData(data string) {
	l.data = data
}
