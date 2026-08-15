package logstream

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/go-redis/redis/v7"
)

type RedisLogStream struct {
	client *redis.Client
}

func NewRedisClient(url string) *RedisLogStream {
	if url == "" {
		log.Println("KAIGARA_REDIS_URL unset, do not connect to redis")
		return &RedisLogStream{client: nil}
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		panic(err)
	}
	client := redis.NewClient(opt)
	_, err = client.Ping().Result()
	if err != nil {
		panic(err)
	}
	log.Println("Connected to redis")
	return &RedisLogStream{client: client}
}

// publishBufSize bounds a single read from the child's pipe. Output longer
// than this is published as several messages.
const publishBufSize = 64 * 1024

func (r *RedisLogStream) Publish(channel string, stream io.ReadCloser) {
	buf := make([]byte, publishBufSize)
	for {
		n, err := stream.Read(buf)

		if n > 0 {
			os.Stdout.Write(buf[:n])

			if r.client != nil {
				// Publish only the bytes just read. Publishing the whole
				// buffer appends whatever the previous, longer read left
				// behind to every short read.
				if e := r.client.Publish(channel, string(buf[:n])).Err(); e != nil {
					// A logging failure must not take the wrapped daemon
					// down with it.
					log.Printf("ERR: failed to publish on %s: %v", channel, e)
				}
			}
		}

		if err != nil {
			// Stop on every error, not just EOF. Returning only on EOF or
			// ErrClosed turns any other persistent read error into a hot
			// loop that spins on a zero-byte read.
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				log.Printf("ERR: reading %s stream: %v", channel, err)
			}
			return
		}
	}
}

// TODO: return a generic type to include Subscribe to the interface
func (r *RedisLogStream) Subscribe(channel string) <-chan *redis.Message {
	return r.client.PSubscribe(channel).Channel()
}

func (r *RedisLogStream) HeartBeat(name string, quit chan int) {
	key := fmt.Sprintf("service.%s", name)

	if r.client != nil {
		r.client.Set(key, time.Now(), 20*time.Second)
	}

	for {
		select {
		case <-quit:
			if r.client != nil {
				r.client.Del(key)
			}
			return

		case <-time.After(10 * time.Second):
			if r.client != nil {
				r.client.Expire(key, 20*time.Second)
			}
		}
	}
}
