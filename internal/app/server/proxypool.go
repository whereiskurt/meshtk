package server

import (
	"bufio"
	"net"
	"sync"
	"time"
)

type ConnectionPool struct {
	mu          sync.Mutex
	pool        []net.Conn
	address     string
	maxSize     int
	currentSize int
	dialTimeout time.Duration
}

type BackendConn struct {
	Conn   net.Conn
	Reader *bufio.Reader
}

type BackendConnectionPool struct {
	mu          sync.Mutex
	pool        []*BackendConn
	address     string
	maxSize     int
	currentSize int
	dialTimeout time.Duration
}

func NewConnectionPool(address string, maxSize int, dialTimeout time.Duration) *ConnectionPool {
	return &ConnectionPool{
		pool:        make([]net.Conn, 0, maxSize),
		address:     address,
		maxSize:     maxSize,
		currentSize: 0,
		dialTimeout: dialTimeout,
	}
}

func (p *ConnectionPool) Get() (net.Conn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.pool) > 0 {
		// Get a connection from the pool
		conn := p.pool[len(p.pool)-1]
		p.pool = p.pool[:len(p.pool)-1]
		return conn, nil
	}

	// No connections in the pool, create a new one if we haven't reached max size
	if p.currentSize < p.maxSize {
		conn, err := net.DialTimeout("tcp", p.address, p.dialTimeout)
		if err != nil {
			return nil, err
		}
		p.currentSize++
		return conn, nil
	}

	// All connections are busy and we've reached max size
	// Wait for a connection to become available (dial with timeout)
	conn, err := net.DialTimeout("tcp", p.address, p.dialTimeout)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (p *ConnectionPool) Put(conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Check if the connection is still valid
	if conn == nil {
		p.currentSize--
		return
	}

	p.pool = append(p.pool, conn)
}

func (p *BackendConnectionPool) Get() (*BackendConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.pool) > 0 {
		// Get a connection from the pool
		backendConn := p.pool[len(p.pool)-1]
		p.pool = p.pool[:len(p.pool)-1]
		return backendConn, nil
	}

	// No connections in the pool, create a new one if we haven't reached max size
	if p.currentSize < p.maxSize {
		conn, err := net.DialTimeout("tcp", p.address, p.dialTimeout)
		if err != nil {
			return nil, err
		}
		p.currentSize++
		return &BackendConn{
			Conn:   conn,
			Reader: bufio.NewReader(conn),
		}, nil
	}

	// All connections are busy and we've reached max size
	// Wait for a connection to become available (dial with timeout)
	conn, err := net.DialTimeout("tcp", p.address, p.dialTimeout)
	if err != nil {
		return nil, err
	}
	return &BackendConn{
		Conn:   conn,
		Reader: bufio.NewReader(conn),
	}, nil
}

func (p *BackendConnectionPool) Put(backendConn *BackendConn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Check if the connection is still valid
	if backendConn == nil || backendConn.Conn == nil {
		p.currentSize--
		return
	}

	// Add the connection back to the pool
	p.pool = append(p.pool, backendConn)
}
