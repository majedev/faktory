package server

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/contribsys/faktory/client"
	"github.com/contribsys/faktory/manager"
	"github.com/contribsys/faktory/storage"
	"github.com/contribsys/faktory/util"
)

type RuntimeStats struct {
	Connections uint64
	Commands    uint64
	StartedAt   time.Time
}

type Server struct {
	Options    *ServerOptions
	Stats      *RuntimeStats
	Subsystems []Subsystem

	listener   net.Listener
	store      storage.Store
	manager    manager.Manager
	workers    *workers
	taskRunner *taskRunner
	mu         sync.Mutex
	stopper    chan bool
	closed     bool
}

func NewServer(opts *ServerOptions) (*Server, error) {
	if opts.Binding == "" {
		opts.Binding = "localhost:7419"
	}
	if opts.StorageDirectory == "" {
		return nil, fmt.Errorf("empty storage directory")
	}

	s := &Server{
		Options:    opts,
		Stats:      &RuntimeStats{StartedAt: time.Now()},
		Subsystems: []Subsystem{},

		stopper: make(chan bool),
		closed:  false,
	}

	return s, nil
}

func (s *Server) Heartbeats() map[string]*ClientData {
	return s.workers.heartbeats
}

func (s *Server) Store() storage.Store {
	return s.store
}

func (s *Server) Manager() manager.Manager {
	return s.manager
}

func (s *Server) Reload() {
	for _, x := range s.Subsystems {
		err := x.Reload(s)
		if err != nil {
			util.Warnf("Subsystem %v returned reload error: %v", x, err)
		}
	}
}

func (s *Server) AddTask(everySec int64, task Taskable) {
	s.taskRunner.AddTask(everySec, task)
}

func (s *Server) Boot() error {
	store, err := storage.Open("redis", s.Options.RedisSock)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", s.Options.Binding)
	if err != nil {
		store.Close()
		return err
	}

	s.mu.Lock()
	s.store = store
	s.workers = newWorkers()
	s.manager = manager.NewManager(store)
	s.listener = listener
	s.stopper = make(chan bool)
	s.startTasks()
	s.mu.Unlock()

	return nil
}

func (s *Server) Run() error {
	if s.store == nil {
		panic("Server hasn't been booted")
	}

	for _, x := range s.Subsystems {
		err := x.Start(s)
		if err != nil {
			return err
		}
	}

	util.Infof("PID %d listening at %s, press Ctrl-C to stop", os.Getpid(), s.Options.Binding)

	// this is the runtime loop for the command server
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return nil
		}
		go func(conn net.Conn) {
			c := startConnection(conn, s)
			if c == nil {
				return
			}
			defer cleanupConnection(s, c)
			s.processLines(c)
		}(conn)
	}
}

func (s *Server) Stopper() chan bool {
	return s.stopper
}

func (s *Server) Stop(f func()) {
	// Don't allow new network connections
	s.mu.Lock()
	s.closed = true
	if s.listener != nil {
		s.listener.Close()
	}
	s.mu.Unlock()

	time.Sleep(100 * time.Millisecond)

	if f != nil {
		f()
	}

	s.store.Close()
}

func cleanupConnection(s *Server, c *Connection) {
	cd, ok := s.workers.heartbeats[c.client.Wid]
	if !ok {
		return
	}
	//util.Debugf("Removing client connection %v", c)
	delete(cd.connections, c)
}

func hash(pwd, salt string, iterations int) string {
	bytes := []byte(pwd + salt)
	hash := sha256.Sum256(bytes)
	if iterations > 1 {
		for i := 1; i < iterations; i++ {
			hash = sha256.Sum256(hash[:])
		}
	}
	return fmt.Sprintf("%x", hash)
}

func startConnection(conn net.Conn, s *Server) *Connection {
	// handshake must complete within 1 second
	conn.SetDeadline(time.Now().Add(1 * time.Second))

	// 4000 iterations is about 1ms on my 2016 MBP w/ 2.9Ghz Core i5
	iter := rand.Intn(4096) + 4000

	var salt string
	conn.Write([]byte(`+HI {"v":2`))
	if s.Options.Password != "" {
		conn.Write([]byte(`,"i":`))
		iters := strconv.FormatInt(int64(iter), 10)
		conn.Write([]byte(iters))
		salt = strconv.FormatInt(rand.Int63(), 16)
		conn.Write([]byte(`,"s":"`))
		conn.Write([]byte(salt))
		conn.Write([]byte(`"}`))
	} else {
		conn.Write([]byte("}"))
	}
	conn.Write([]byte("\r\n"))

	buf := bufio.NewReader(conn)

	line, err := buf.ReadString('\n')
	if err != nil {
		util.Error("Closing connection", err)
		conn.Close()
		return nil
	}

	valid := strings.HasPrefix(line, "HELLO {")
	if !valid {
		util.Infof("Invalid preamble: %s", line)
		util.Info("Need a valid HELLO")
		conn.Close()
		return nil
	}

	client, err := clientDataFromHello(line[5:])
	if err != nil {
		util.Error("Invalid client data in HELLO", err)
		conn.Close()
		return nil
	}

	if s.Options.Password != "" {
		if client.Version < 2 {
			iter = 1
		}

		if subtle.ConstantTimeCompare([]byte(client.PasswordHash), []byte(hash(s.Options.Password, salt, iter))) != 1 {
			conn.Write([]byte("-ERR Invalid password\r\n"))
			conn.Close()
			return nil
		}
	}

	cn := &Connection{
		client: client,
		conn:   conn,
		buf:    buf,
	}

	if client.Wid == "" {
		// a producer, not a consumer connection
	} else {
		cd, _ := s.workers.heartbeat(client, true)
		cd.connections[cn] = true
	}

	_, err = conn.Write([]byte("+OK\r\n"))
	if err != nil {
		util.Error("Closing connection", err)
		conn.Close()
		return nil
	}

	// disable deadline
	conn.SetDeadline(time.Time{})

	return cn
}

func (s *Server) processLines(conn *Connection) {
	atomic.AddUint64(&s.Stats.Connections, 1)
	defer atomic.AddUint64(&s.Stats.Connections, ^uint64(0))

	for {
		cmd, e := conn.buf.ReadString('\n')
		if e != nil {
			if e != io.EOF {
				util.Error("Unexpected socket error", e)
			}
			conn.Close()
			return
		}
		if s.closed {
			conn.Error("Closing connection", newTaggedError("SHUTDOWN", fmt.Errorf("Shutdown in progress")))
			conn.Close()
			return
		}
		cmd = strings.TrimSuffix(cmd, "\r\n")
		cmd = strings.TrimSuffix(cmd, "\n")
		//util.Debug(cmd)

		idx := strings.Index(cmd, " ")
		verb := cmd
		if idx >= 0 {
			verb = cmd[0:idx]
		}
		proc, ok := cmdSet[verb]
		if !ok {
			conn.Error(cmd, fmt.Errorf("Unknown command %s", verb))
		} else {
			atomic.AddUint64(&s.Stats.Commands, 1)
			proc(conn, s, cmd)
		}
		if verb == "END" {
			break
		}
	}
}

func (s *Server) uptimeInSeconds() int {
	return int(time.Since(s.Stats.StartedAt).Seconds())
}

func (s *Server) CurrentState() (map[string]interface{}, error) {
	defalt, err := s.store.GetQueue("default")
	if err != nil {
		return nil, err
	}

	totalQueued := 0
	totalQueues := 0
	// queue size is cached so this should be very efficient.
	s.store.EachQueue(func(q storage.Queue) {
		totalQueued += int(q.Size())
		totalQueues++
	})

	return map[string]interface{}{
		"server_utc_time": time.Now().UTC().Format("03:04:05 UTC"),
		"faktory": map[string]interface{}{
			"default_size":    defalt.Size(),
			"total_failures":  s.store.TotalFailures(),
			"total_processed": s.store.TotalProcessed(),
			"total_enqueued":  totalQueued,
			"total_queues":    totalQueues,
			"tasks":           s.taskRunner.Stats()},
		"server": map[string]interface{}{
			"faktory_version": client.Version,
			"uptime":          s.uptimeInSeconds(),
			"connections":     atomic.LoadUint64(&s.Stats.Connections),
			"command_count":   atomic.LoadUint64(&s.Stats.Commands),
			"used_memory_mb":  util.MemoryUsage()},
	}, nil
}
