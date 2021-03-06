package hub

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalTransportWriteIsNotDispatchedUntilListen(t *testing.T) {
	transport := NewLocalTransport(5, time.Second)
	defer transport.Close()
	assert.Implements(t, (*Transport)(nil), transport)

	err := transport.Write(&Update{})
	assert.Nil(t, err)

	pipe, err := transport.CreatePipe("")
	assert.Nil(t, err)
	require.NotNil(t, pipe)

	var (
		readUpdate *Update
		ok         bool
		m          sync.Mutex
		wg         sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		m.Lock()
		defer m.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		go wg.Done()

		select {
		case readUpdate, ok = <-pipe.Read():
		case <-ctx.Done():
		}
	}()

	wg.Wait()
	pipe.Close()

	m.Lock()
	defer m.Unlock()
	assert.Nil(t, readUpdate)
	assert.False(t, ok)
}

func TestLocalTransportWriteIsDispatched(t *testing.T) {
	transport := NewLocalTransport(5, time.Second)
	defer transport.Close()
	assert.Implements(t, (*Transport)(nil), transport)

	pipe, err := transport.CreatePipe("")
	assert.Nil(t, err)
	require.NotNil(t, pipe)
	defer pipe.Close()

	var (
		readUpdate *Update
		ok         bool
		m          sync.Mutex
		wg         sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		m.Lock()
		defer m.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		go wg.Done()
		select {
		case readUpdate, ok = <-pipe.Read():
		case <-ctx.Done():
		}
	}()

	wg.Wait()
	err = transport.Write(&Update{})
	assert.Nil(t, err)

	m.Lock()
	defer m.Unlock()

	assert.True(t, ok)
	assert.NotNil(t, readUpdate)
}

func TestLocalTransportClosed(t *testing.T) {
	transport := NewLocalTransport(5, time.Second)
	defer transport.Close()
	assert.Implements(t, (*Transport)(nil), transport)

	pipe, _ := transport.CreatePipe("")
	require.NotNil(t, pipe)

	err := transport.Close()
	assert.Nil(t, err)

	_, err = transport.CreatePipe("")
	assert.Equal(t, err, ErrClosedTransport)

	err = transport.Write(&Update{})
	assert.Equal(t, err, ErrClosedTransport)

	_, ok := <-pipe.Read()
	assert.False(t, ok)
}

func TestLiveCleanClosedPipes(t *testing.T) {
	transport := NewLocalTransport(5, time.Second)
	defer transport.Close()

	pipe, _ := transport.CreatePipe("")
	require.NotNil(t, pipe)

	assert.Len(t, transport.pipes, 1)

	pipe.Close()
	assert.Len(t, transport.pipes, 1)

	transport.Write(&Update{})
	assert.Len(t, transport.pipes, 0)
}

func TestLivePipeReadingBlocks(t *testing.T) {
	transport := NewLocalTransport(5, time.Second)
	defer transport.Close()
	assert.Implements(t, (*Transport)(nil), transport)

	pipe, err := transport.CreatePipe("")
	assert.Nil(t, err)
	require.NotNil(t, pipe)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		wg.Wait()
		err := transport.Write(&Update{})
		assert.Nil(t, err)
	}()

	wg.Done()
	u, ok := <-pipe.Read()
	assert.True(t, ok)
	assert.NotNil(t, u)
}

func TestNewTransport(t *testing.T) {
	transport, err := NewTransport(viper.New())
	assert.Nil(t, err)
	require.NotNil(t, transport)
	transport.Close()
	assert.IsType(t, &LocalTransport{}, transport)

	v := viper.New()
	v.Set("transport_url", "bolt://test.db")
	transport, _ = NewTransport(v)
	assert.Nil(t, err)
	require.NotNil(t, transport)
	transport.Close()
	os.Remove("test.db")
	assert.IsType(t, &BoltTransport{}, transport)

	v = viper.New()
	v.Set("transport_url", "nothing:")
	transport, err = NewTransport(v)
	assert.Nil(t, transport)
	assert.NotNil(t, err)
	assert.EqualError(t, err, `"nothing:": no such transport available: invalid transport DSN`)

	v = viper.New()
	v.Set("transport_url", "http://[::1]%23")
	_, err = NewTransport(v)
	assert.EqualError(t, err, `transport_url: parse "http://[::1]%23": invalid port "%23" after host`)
}
