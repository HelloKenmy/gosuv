package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/go-yaml/yaml"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/qiniu/log"
)

var defaultConfigDir string

func init() {
	defaultConfigDir = os.Getenv("GOSUV_HOME_DIR")
	if defaultConfigDir == "" {
		defaultConfigDir = filepath.Join(UserHomeDir(), ".gosuv")
	}
}

type Supervisor struct {
	ConfigDir string

	names   []string
	pgMap   map[string]Program
	procMap map[string]*Process
	mu      sync.Mutex
	eventB  *WriteBroadcaster
}

func (s *Supervisor) programs() []Program {
	pgs := make([]Program, 0, len(s.names))
	for _, name := range s.names {
		pgs = append(pgs, s.pgMap[name])
	}
	return pgs
}

func (s *Supervisor) procs() []*Process {
	ps := make([]*Process, 0, len(s.names))
	for _, name := range s.names {
		ps = append(ps, s.procMap[name])
	}
	return ps
}

func (s *Supervisor) programPath() string {
	return filepath.Join(s.ConfigDir, "programs.yml")
}

func (s *Supervisor) newProcess(pg Program) *Process {
	p := NewProcess(pg)
	origFunc := p.StateChange
	p.StateChange = func(oldState, newState FSMState) {
		s.broadcastEvent(fmt.Sprintf("%s state: %s -> %s", p.Name, string(oldState), string(newState)))
		origFunc(oldState, newState)
	}
	return p
}

func (s *Supervisor) broadcastEvent(event string) {
	s.eventB.Write([]byte(event))
}

func (s *Supervisor) addStatusChangeListener(c chan string) {
	sChan := s.eventB.NewChanString(fmt.Sprintf("%d", time.Now().UnixNano()))
	go func() {
		for msg := range sChan {
			c <- msg
		}
	}()
}

// Send Stop signal and wait program stops
func (s *Supervisor) stopAndWait(name string) error {
	p, ok := s.procMap[name]
	if !ok {
		return errors.New("No such program")
	}
	if !p.IsRunning() {
		return nil
	}
	c := make(chan string, 0)
	s.addStatusChangeListener(c)
	p.Operate(StopEvent)
	for {
		select {
		case <-c:
			if !p.IsRunning() {
				return nil
			}
		case <-time.After(1 * time.Second): // In case some event not catched
			if !p.IsRunning() {
				return nil
			}
		}
	}
}

func (s *Supervisor) addOrUpdateProgram(pg Program) error {
	// defer s.broadcastEvent(pg.Name + " add or update")
	if err := pg.Check(); err != nil {
		return err
	}
	origPg, ok := s.pgMap[pg.Name]
	if ok {
		if reflect.DeepEqual(origPg, pg) {
			return nil
		}
		s.broadcastEvent(pg.Name + " update")
		log.Println("Update:", pg.Name)
		origProc := s.procMap[pg.Name]
		isRunning := origProc.IsRunning()
		go func() {
			s.stopAndWait(origProc.Name)

			newProc := s.newProcess(pg)
			s.procMap[pg.Name] = newProc
			s.pgMap[pg.Name] = pg // update origin
			if isRunning {
				newProc.Operate(StartEvent)
			}
		}()
	} else {
		// s.pgs = append(s.pgs, &pg)
		s.names = append(s.names, pg.Name)
		s.pgMap[pg.Name] = pg
		s.procMap[pg.Name] = s.newProcess(pg)
		s.broadcastEvent(pg.Name + " added")
	}
	return nil
}

// Check
// - Yaml format
// - Duplicated program
func (s *Supervisor) readConfigFromDB() (pgs []Program, err error) {
	data, err := ioutil.ReadFile(s.programPath())
	if err != nil {
		data = []byte("")
	}
	pgs = make([]Program, 0)
	if err = yaml.Unmarshal(data, &pgs); err != nil {
		return nil, err
	}
	visited := map[string]bool{}
	for _, pg := range pgs {
		if visited[pg.Name] {
			return nil, fmt.Errorf("Duplicated program name: %s", pg.Name)
		}
		visited[pg.Name] = true
	}
	return
}

func (s *Supervisor) loadDB() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	pgs, err := s.readConfigFromDB()
	if err != nil {
		return err
	}
	// add or update program
	visited := map[string]bool{}
	names := make([]string, 0, len(pgs))
	for _, pg := range pgs {
		names = append(names, pg.Name)
		visited[pg.Name] = true
		s.addOrUpdateProgram(pg)
	}
	s.names = names
	// delete not exists program
	for _, pg := range s.pgMap {
		if visited[pg.Name] {
			continue
		}
		name := pg.Name
		log.Printf("stop before delete program: %s", name)
		s.stopAndWait(name)
		delete(s.procMap, name)
		delete(s.pgMap, name)
		s.broadcastEvent(pg.Name + " deleted")
	}
	return nil
}

func (s *Supervisor) saveDB() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := yaml.Marshal(s.programs())
	if err != nil {
		return err
	}
	return ioutil.WriteFile(s.programPath(), data, 0644)
}

type WebConfig struct {
	User    string
	Version string
}

func (s *Supervisor) renderHTML(w http.ResponseWriter, name string, data interface{}) {
	w.Header().Set("Content-Type", "text/html")
	wc := WebConfig{}
	wc.Version = Version
	user, err := user.Current()
	if err == nil {
		wc.User = user.Username
	}
	if data == nil {
		data = wc
	}
	executeTemplate(w, name, data)
}

type JSONResponse struct {
	Status int         `json:"status"`
	Value  interface{} `json:"value"`
}

func (s *Supervisor) renderJSON(w http.ResponseWriter, data JSONResponse) {
	w.Header().Set("Content-Type", "application/json")
	bytes, _ := json.Marshal(data)
	w.Write(bytes)
}

func (s *Supervisor) hIndex(w http.ResponseWriter, r *http.Request) {
	s.renderHTML(w, "index", nil)
}

func (s *Supervisor) hSetting(w http.ResponseWriter, r *http.Request) {
	s.renderHTML(w, "setting", nil)
}

func (s *Supervisor) hStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	data, _ := json.Marshal(map[string]interface{}{
		"status": 0,
		"value":  "server is running",
	})
	w.Write(data)
}

func (s *Supervisor) hShutdown(w http.ResponseWriter, r *http.Request) {
	s.Close()
	s.renderJSON(w, JSONResponse{
		Status: 0,
		Value:  "gosuv server has been shutdown",
	})
	go func() {
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	}()
}

func (s *Supervisor) hReload(w http.ResponseWriter, r *http.Request) {
	err := s.loadDB()
	log.Println("reload config file")
	if err == nil {
		s.renderJSON(w, JSONResponse{
			Status: 0,
			Value:  "load config success",
		})
	} else {
		s.renderJSON(w, JSONResponse{
			Status: 1,
			Value:  err.Error(),
		})
	}
}

func (s *Supervisor) hGetProgram(w http.ResponseWriter, r *http.Request) {
	data, err := json.Marshal(s.procs())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *Supervisor) hAddProgram(w http.ResponseWriter, r *http.Request) {
	retries, err := strconv.Atoi(r.FormValue("retries"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	pg := Program{
		Name:         r.FormValue("name"),
		Command:      r.FormValue("command"),
		Dir:          r.FormValue("dir"),
		StartAuto:    r.FormValue("autostart") == "on",
		StartRetries: retries,
		// TODO: missing other values
	}
	if pg.Dir == "" {
		pg.Dir = "/"
	}
	if err := pg.Check(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	var data []byte
	if _, ok := s.pgMap[pg.Name]; ok {
		data, _ = json.Marshal(map[string]interface{}{
			"status": 1,
			"error":  fmt.Sprintf("Program %s already exists", strconv.Quote(pg.Name)),
		})
	} else {
		if err := s.addOrUpdateProgram(pg); err != nil {
			data, _ = json.Marshal(map[string]interface{}{
				"status": 1,
				"error":  err.Error(),
			})
		} else {
			s.saveDB()
			data, _ = json.Marshal(map[string]interface{}{
				"status": 0,
			})
		}
	}
	w.Write(data)
}

func (s *Supervisor) hStartProgram(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	proc, ok := s.procMap[name]
	var data []byte
	if !ok {
		data, _ = json.Marshal(map[string]interface{}{
			"status": 1,
			"error":  fmt.Sprintf("Process %s not exists", strconv.Quote(name)),
		})
	} else {
		proc.Operate(StartEvent)
		data, _ = json.Marshal(map[string]interface{}{
			"status": 0,
			"name":   name,
		})
	}
	w.Write(data)
}

func (s *Supervisor) hStopProgram(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	proc, ok := s.procMap[name]
	var data []byte
	if !ok {
		data, _ = json.Marshal(map[string]interface{}{
			"status": 1,
			"error":  fmt.Sprintf("Process %s not exists", strconv.Quote(name)),
		})
	} else {
		proc.Operate(StopEvent)
		data, _ = json.Marshal(map[string]interface{}{
			"status": 0,
			"name":   name,
		})
	}
	w.Write(data)
}

var upgrader = websocket.Upgrader{}

func (s *Supervisor) wsEvents(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Print("upgrade:", err)
		return
	}
	defer c.Close()

	ch := make(chan string, 0)
	s.addStatusChangeListener(ch)
	go func() {
		for message := range ch {
			// Question: type 1 ?
			c.WriteMessage(1, []byte(message))
		}
		// s.eventB.RemoveListener(ch)
	}()
	for {
		mt, message, err := c.ReadMessage()
		if err != nil {
			log.Println("read:", mt, err)
			break
		}
		log.Printf("recv: %v %s", mt, message)
		err = c.WriteMessage(mt, message)
		if err != nil {
			log.Println("write:", err)
			break
		}
	}
}

func (s *Supervisor) wsLog(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	log.Println(name)
	proc, ok := s.procMap[name]
	if !ok {
		log.Println("No such process")
		// TODO: raise error here?
		return
	}

	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Print("upgrade:", err)
		return
	}
	defer c.Close()

	for data := range proc.Output.NewChanString(r.RemoteAddr) {
		err := c.WriteMessage(1, []byte(data))
		if err != nil {
			proc.Output.CloseWriter(r.RemoteAddr)
			break
		}
	}
}

func (s *Supervisor) Close() {
	for _, proc := range s.procMap {
		s.stopAndWait(proc.Name)
	}
	log.Println("server closed")
}

func (s *Supervisor) catchExitSignal() {
	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for sig := range sigC {
			if sig == syscall.SIGHUP {
				log.Println("Receive SIGHUP, just ignore")
				continue
			}
			log.Printf("Got signal: %v, stopping all running process\n", sig)
			s.Close()
			break
		}
		os.Exit(0)
	}()
}

func newSupervisorHandler() (hdlr http.Handler, err error) {
	suv := &Supervisor{
		ConfigDir: defaultConfigDir,
		pgMap:     make(map[string]Program, 0),
		procMap:   make(map[string]*Process, 0),
		eventB:    NewWriteBroadcaster(4 * 1024),
	}
	if err = suv.loadDB(); err != nil {
		return
	}
	suv.catchExitSignal()

	r := mux.NewRouter()
	r.HandleFunc("/", suv.hIndex)
	r.HandleFunc("/settings/{name}", suv.hSetting)

	r.HandleFunc("/api/status", suv.hStatus)
	r.HandleFunc("/api/shutdown", suv.hShutdown).Methods("POST")
	r.HandleFunc("/api/reload", suv.hReload).Methods("POST")

	r.HandleFunc("/api/programs", suv.hGetProgram).Methods("GET")
	r.HandleFunc("/api/programs", suv.hAddProgram).Methods("POST")
	r.HandleFunc("/api/programs/{name}/start", suv.hStartProgram).Methods("POST")
	r.HandleFunc("/api/programs/{name}/stop", suv.hStopProgram).Methods("POST")

	r.HandleFunc("/ws/events", suv.wsEvents)
	r.HandleFunc("/ws/logs/{name}", suv.wsLog)

	return r, nil
}
