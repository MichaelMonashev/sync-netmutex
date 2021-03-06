// Package netmutex implements low-level high performance client library for Taooka distributed lock manager (http://taooka.com/).
// It is very important to correctly handle errors that return functions!!!
package netmutex

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MichaelMonashev/sync/netmutex/code"
)

// Size limits.
const (
	// MaxKeySize is the maximum size of the key.
	MaxKeySize = 255

	// MaxIsolationInfo - the maximum length of information for client isolation. Taooka passes it to the STDIN program "isolate", while the client is broken.
	MaxIsolationInfo = 400
)

// Returned errors.
var (
	// ErrDisconnected - connection is closed.
	ErrDisconnected = errors.New("Client connection had closed.")

	// ErrIsolated - the client is isolated. You need to quit the program.
	ErrIsolated = errors.New("Client had isolated.")

	// ErrLocked - the key is locked by someone else.
	ErrLocked = errors.New("Key locked.")

	// ErrNoServers - could not connect to any server from the list or all of them became unavailable.
	ErrNoServers = errors.New("No working servers.")

	// ErrTooMuchRetries - the number of attempts to send a request to the server has been exceeded.
	ErrTooMuchRetries = errors.New("Too much retries.")

	// ErrLongKey - the key is longer than MaxKeySize bytes.
	ErrLongKey = errors.New("Key too long.")

	// ErrWrongTTL - TTL is less than zero.
	ErrWrongTTL = errors.New("Wrong TTL.")

	// ErrLongIsolationInfo - information for client isolation is longer than MaxIsolationInfo bytes.
	ErrLongIsolationInfo = errors.New("Client isolation information too long.")
)

var (
	errWrongResponse = errors.New("Wrong response.")
	errWrongLock     = errors.New("Use m := conn.NewMutex() instead of m := &Mutex{}.")
)

type servers struct {
	sync.Mutex
	m       map[uint64]*server
	current *server
}

// Mutex — mutex object.
type Mutex struct {
	key       string
	commandID commandID
	conn      *Conn
}

// NewMutex return new mutex object.
func (conn *Conn) NewMutex() *Mutex {
	return &Mutex{
		conn: conn,
	}
}

// Options specifies additional connection parameters.
type Options struct {
	IsolationInfo string // Information about how the client will be isolated from the data it is changing in case of non-operation.
}

// Conn — connection to the distributed lock manager.
type Conn struct {
	nextCommandID   commandID // должна быть первым полем в структуре, иначе может быть неверное выравнивание и atomic перестанет работать
	done            chan struct{}
	servers         *servers
	workingCommands *workingCommands
}

// Open tries during timeout to connect to any server from the list of addrs, get the current list of servers from it using options. If not, then repeat the bypass retries once. If so, then it tries to open a connection to each server from the list of servers received.
func Open(retries int, timeout time.Duration, addrs []string, options *Options) (*Conn, error) {

	conn := &Conn{
		done:            make(chan struct{}),
		workingCommands: newWorkingCommands(),
	}

	isolationInfo := ""
	if options != nil {
		isolationInfo = options.IsolationInfo
	}

	if len(isolationInfo) > MaxIsolationInfo {
		return nil, ErrLongIsolationInfo
	}

	// обходим все сервера из списка, пока не найдём доступный
	for i := 0; i < retries; i++ {
		for _, addr := range addrs {
			resp, err := conn.connect(addr, timeout, isolationInfo)
			if err != nil {
				continue
			}
			conn.nextCommandID = resp.id

			remoteServers := make(map[uint64]*server)

			// пробуем соединиться с серверами из полученного в ответе списка,
			// отправить им PING, получить OK, тем самым проверив прохождение пакетов
			// а у не прошедших проверку серверов увеличить Fails
			for serverID, serverAddr := range resp.servers {

				s := &server{
					id:   serverID,
					addr: serverAddr,
				}

				remoteServers[serverID] = s

				s.conn, err = openConn(s.addr)
				if err != nil {
					s.fail()
					continue
				}

				err = conn.ping(s, timeout)
				if err != nil {
					s.fail()
				}
			}

			conn.servers = &servers{
				m: remoteServers,
			}

			// запускаем горутины, ждущие ответов от каждого сервера
			for _, server := range conn.servers.m {

				if server.conn != nil {
					go conn.readResponses(server)
				} else {
					go conn.repairConn(server)
				}
			}

			return conn, nil
		}
	}

	return nil, ErrNoServers
}

// Lock tries to lock the key, making no more retries of attempts, during each waiting for a response from the server during the timeout. If the lock succeeds, it is written to m.
func (m *Mutex) Lock(retries int, timeout time.Duration, key string, ttl time.Duration) (uint64, error) {

	if m.conn == nil {
		return 0, errWrongLock
	}

	if len(key) > MaxKeySize {
		return 0, ErrLongKey
	}

	if ttl < 0 {
		return 0, ErrWrongTTL
	}

	conn := m.conn

	id := conn.commandID()

	fenceID, err := conn.runCommand2(key, id, code.LOCK, timeout, ttl, commandID{}, retries)
	if err != nil {
		return 0, err
	}

	m.key = key
	m.commandID = id

	return fenceID, nil

}

// Update tries to update the ttl of the m mutex, making no more retries of attempts, during each waiting for a response from the server during the timeout. Allows you to extend the lifetime of the lock. Suitable for the implementation of heartbeat, which allows optimal control of the ttl key.
func (m *Mutex) Update(retries int, timeout time.Duration, ttl time.Duration) error {
	if m.conn == nil {
		return errWrongLock
	}

	if len(m.key) > MaxKeySize {
		return ErrLongKey
	}

	if ttl < 0 {
		return ErrWrongTTL
	}

	conn := m.conn

	return conn.runCommand(m.key, conn.commandID(), code.UPDATE, timeout, ttl, m.commandID, retries)
}

//Unlock tries to unlock m, making no more retries of attempts, during each waiting for a response from the server during the timeout.
func (m *Mutex) Unlock(retries int, timeout time.Duration) error {
	if m.conn == nil {
		return errWrongLock
	}

	if len(m.key) > MaxKeySize {
		return ErrLongKey
	}

	conn := m.conn

	return conn.runCommand(m.key, conn.commandID(), code.UNLOCK, timeout, 0, m.commandID, retries)
}

//UnlockAll tries to remove all locks by making no more retries of attempts, during each waiting for a response from the server during the timeout.
//Use with caution! Clients with existing locks will be isolated!
func (conn *Conn) UnlockAll(retries int, timeout time.Duration) error {
	return conn.runCommand("", conn.commandID(), code.UNLOCKALL, timeout, 0, commandID{}, retries)
}

// Close closes the connection to the distributed lock manager.
func (conn *Conn) Close(retries int, timeout time.Duration) error {
	defer close(conn.done)
	return conn.runCommand("", conn.commandID(), code.DISCONNECT, timeout, 0, commandID{}, retries)
}

func (conn *Conn) runCommand(key string, id commandID, code byte, timeout time.Duration, ttl time.Duration, lockID commandID, retries int) error {

	req := getRequest()
	defer putRequest(req)

	req.id = id
	req.code = code
	req.key = key
	req.timeout = timeout
	req.ttl = ttl
	req.lockID = lockID
	req.retries = retries
	req.conn = conn

	conn.workingCommands.add(req)
	defer conn.workingCommands.delete(req.id)

	go req.run()

	return <-req.respChan
}

func (conn *Conn) runCommand2(key string, id commandID, code byte, timeout time.Duration, ttl time.Duration, lockID commandID, retries int) (uint64, error) {

	req := getRequest()
	defer putRequest(req)

	req.id = id
	req.code = code
	req.key = key
	req.timeout = timeout
	req.ttl = ttl
	req.lockID = lockID
	req.retries = retries
	req.conn = conn

	conn.workingCommands.add(req)
	defer conn.workingCommands.delete(req.id)

	go req.run()

	err := <-req.respChan

	return req.fenceID, err
}

func (conn *Conn) touch(s *server) {
	req := getRequest()
	defer putRequest(req)

	req.code = code.TOUCH

	for {
		time.Sleep(10 * time.Minute) // ToDo Вынести в константы

		// выходим из цикла, если клиент закончил свою работу
		select {
		case <-conn.done:
			return
		default:
		}

		req.id = conn.commandID()

		write(s, req) // ответ именно для этого req.id нам не важен, так что не запускаем горутину, ждущую именно этот ответ.
	}
}

//  горутины (по числу серверов) читают ответы из своих соединений и направляют их в канал ответов
func (conn *Conn) readResponses(s *server) {

	go conn.touch(s)

	for {
		// выходим из цикла, если клиент закончил свою работу
		select {
		case <-conn.done:
			s.conn.Close()
			return
		default:
		}

		// таймаут нужен для того, чтобы не залипнуть в чтении навечно, а можно было иногда от туда возвращаться,
		// например, чтобы корректно закончить работу клиента

		// Optimization: see https://github.com/golang/go/issues/15133 for details.
		currentTime := time.Now()
		if currentTime.Sub(s.lastReadDeadlineTime) > 59*time.Second {
			s.lastReadDeadlineTime = currentTime
			err := s.conn.SetReadDeadline(currentTime.Add(time.Minute))
			if err != nil {
				s.fail()
				continue
			}
		}

		resp, err := read(s)

		// если произошёл таймаут, выставленный строчкой выше, или ошибка временная
		if netErr, ok := err.(*net.OpError); ok {
			if netErr.Timeout() || netErr.Temporary() {
				continue
			}
		}

		if err != nil {
			// пример ошибки: read udp 127.0.0.1:19858->127.0.0.1:3002: read: connection refused
			s.fail()
			continue
		}

		// OPTIONS не привязана ни к какому запросу, поэтому обрабатывается отдельно
		if resp.code == code.OPTIONS {
			// переконфигурация: новый список сервероов, новый уникальный commandID
			// ToDo написать переконфигурацию
			putResponse(resp)
			continue
		}

		// находим запрос, соотвествующий ответу
		req, ok := conn.workingCommands.get(resp.id)
		// если команда не нашлась по ID, то ждём следующую
		if !ok {
			putResponse(resp)
			continue
		}
		req.processChan <- resp

	}
}

// пытается открыть соединение
func (conn *Conn) repairConn(server *server) {
	for {
		select {
		case <-conn.done:
			// выходим из цикла, если клиент закончил свою работу
			return

		default:
		}

		c, err := openConn(server.addr)

		if err != nil {
			server.fail()
			time.Sleep(time.Minute)
			continue
		}
		server.conn = c

		go conn.readResponses(server)
		return
	}
}

func (conn *Conn) connect(addr string, timeout time.Duration, isolationInfo string) (*response, error) {
	c, err := openConn(addr)
	if err != nil {
		return nil, err
	}

	defer c.Close()

	s := &server{
		id:   0,
		addr: addr,
		conn: c,
	}

	req := &request{
		code:          code.CONNECT,
		isolationInfo: isolationInfo,
	}

	err = write(s, req)
	if err != nil {
		return nil, err
	}

	resp, err := readWithTimeout(s, timeout) // ToDo: вынести таймаут в Опции
	if err != nil {
		return nil, err
	}

	if resp.code != code.OPTIONS {
		return nil, errWrongResponse
	}

	return resp, nil
}

func (conn *Conn) ping(s *server, timeout time.Duration) error {
	req := &request{
		code: code.PING,
		id:   conn.commandID(),
	}

	err := write(s, req)
	if err != nil {
		return err
	}

	resp, err := readWithTimeout(s, timeout)
	if err != nil {
		return err
	}

	if resp.code != code.PONG || resp.id != req.id {
		return errWrongResponse
	}

	return nil
}

func (conn *Conn) commandID() commandID {
	return commandID{
		connectionID: conn.nextCommandID.connectionID,
		requestID:    atomic.AddUint64(&conn.nextCommandID.requestID, 1),
	}
}

// возвращает лучший из возможных серверов
func (conn *Conn) server() (*server, error) {
	conn.servers.Lock()
	defer conn.servers.Unlock()

	if conn.servers.current != nil {
		if atomic.LoadUint32(&conn.servers.current.fails) == 0 { // ToDo переписать. при множестве потерь пакетов это условие редко срабатывает и каждый раз делается перебор серверов
			return conn.servers.current, nil
		}
	}

	var bestFails uint32
	var bestServer *server
	for _, s := range conn.servers.m {
		// пропускаем несоединившиеся серверы
		if s.conn == nil {
			continue
		}

		// пропускаем текущий сервер
		if conn.servers.current == s {
			continue
		}

		if bestServer == nil {
			bestFails = atomic.LoadUint32(&s.fails)
			bestServer = s
		} else {
			curValue := atomic.LoadUint32(&s.fails)
			if bestFails > curValue {
				bestFails = curValue
				bestServer = s
			}
		}
	}

	if bestServer != nil {
		conn.servers.current = bestServer
		return conn.servers.current, nil
	}
	// если лучшего сервера не нашлось, а текущий имеется, то используем текущий сервер
	if conn.servers.current != nil {
		return conn.servers.current, nil
	}

	return nil, ErrNoServers
}

func (conn *Conn) serverByID(serverID uint64) *server {
	conn.servers.Lock()
	defer conn.servers.Unlock()

	if server, ok := conn.servers.m[serverID]; ok {
		return server
	}

	return nil
}
