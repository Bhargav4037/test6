package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	"cloud.google.com/go/cloudsqlconn/postgres/pgxv4"
	"github.com/gomodule/redigo/redis"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"

	_ "github.com/jackc/pgx/v4/stdlib"
)

// SQLStorage is a wrapper for database operations
type SQLStorage struct {
	db *sql.DB
}

func (s *SQLStorage) log(msg string) {
	log.Printf("sql       : %s\n", msg)
}

// Init kicks off the database connector
func (s *SQLStorage) Init(user, password, host, name, conn string) error {
	var err error
	instanceConnectionName := conn

	s.log("Opening connection")

	if password == "" {
		s.log("method is service account")
		trimmedUser := strings.ReplaceAll(user, ".gserviceaccount.com", "")
		if s.db, err = connectWithConnector(trimmedUser, password, name, instanceConnectionName); err != nil {
			return fmt.Errorf("could not open connection using Service Account: %s", err)
		}
	} else {
		s.log("method is database user")
		if s.db, err = connectDirect(user, password, name, host); err != nil {
			return fmt.Errorf("could open connection using username/password: %s", err)
		}
	}

	s.log("Connection opened")

	s.log("Pinging")
	if err := s.db.Ping(); err != nil {
		return fmt.Errorf("could not ping database: %s", err)
	}
	s.log("Pinging complete")

	populated, err := s.SchemaExists()
	if err != nil {
		return fmt.Errorf("schema exists failure: %s", err)
	}

	if !populated {
		s.log("populating schema")
		if err := s.SchemaInit(); err != nil {
			return fmt.Errorf("cannot populate schema: %s", err)
		}
	}

	s.log("Schema populated")

	return nil
}

func connectWithConnector(user, pass, name, connection string) (*sql.DB, error) {
	cleanup, err := pgxv4.RegisterDriver(
		"cloudsql-postgres",
		cloudsqlconn.WithDefaultDialOptions(cloudsqlconn.WithPrivateIP()),
		cloudsqlconn.WithIAMAuthN(),
	)
	if err != nil {
		log.Fatalf("uncaught error occured: %s", err)
	}
	defer cleanup()

	connectString := fmt.Sprintf("host=%s user=%s dbname=%s sslmode=disable", connection, user, name)

	return sql.Open(
		"cloudsql-postgres",
		connectString,
	)
}

func connectDirect(user, pass, name, connection string) (*sql.DB, error) {
	connectString := fmt.Sprintf("host=%s user=%s password=%s port=%s dbname=%s sslmode=disable", connection, user, pass, "5432", name)

	return sql.Open("pgx", connectString)
}

// Close ends the database connection
func (s *SQLStorage) Close() error {
	s.log("close called on database")
	return s.db.Close()
}

// SchemaExists checks to see if the schema has been prepopulated
func (s SQLStorage) SchemaExists() (bool, error) {
	s.log("Checking schema exists")
	var result string
	err := s.db.QueryRow(`SELECT
    table_schema || '.' || table_name
FROM
    information_schema.tables
WHERE
    table_type = 'BASE TABLE'
AND
    table_schema NOT IN ('pg_catalog', 'information_schema');`).Scan(&result)
	if err != nil && err != sql.ErrNoRows {
		return false, fmt.Errorf("schema check failed: %s", err)
	}
	s.log("Schema check complete")
	s.log(fmt.Sprintf("Schema err: %s", err))
	return len(result) > 0, nil
}

// SchemaInit will initialize the schema
func (s *SQLStorage) SchemaInit() error {
	sl := make([]string, 0)

	sl = append(sl, `CREATE TABLE todo (
		id SERIAL PRIMARY KEY,
		title varchar(512) DEFAULT NULL,
		updated timestamp DEFAULT NULL,
		completed timestamp DEFAULT NULL)`)
	sl = append(sl, `INSERT INTO todo (id, title, updated, completed)
					VALUES
	  				(1,'Install and configure todo app','2021-10-28 12:00:00','2021-10-28 12:00:00'),
					(2,'Add your own todo','2021-10-28 12:00:00',NULL),
					(3,'Mark task 1 done','2021-10-27 14:26:00',NULL)`)

	sl = append(sl, `SELECT setval('todo_id_seq', (SELECT MAX(id) FROM todo)+1)`)

	// Get new Transaction. See http://golang.org/pkg/database/sql/#DB.Begin
	txn, err := s.db.Begin()
	if err != nil {
		return err
	}

	defer func() {
		// Rollback the transaction after the function returns.
		// If the transaction was already commited, this will do nothing.
		_ = txn.Rollback()
	}()

	for _, q := range sl {
		// Execute the query in the transaction.
		s.log(fmt.Sprintf("Executing sql: %s", q))
		_, err := txn.Exec(q)
		if err != nil {
			return err
		}
	}

	// Commit the transaction.
	return txn.Commit()
}

// List returns a list of all todos
func (s SQLStorage) List() (Todos, error) {
	ts := Todos{}
	results, err := s.db.Query("SELECT * FROM todo ORDER BY updated DESC")
	if err != nil {
		return ts, fmt.Errorf("list error: on query: %s", err)
	}

	for results.Next() {
		t, err := resultToTodo(results)
		if err != nil {
			return ts, fmt.Errorf("list error: on resultToTodo: %s", err)
		}

		ts = append(ts, t)
	}
	return ts, nil
}

// Create records a new todo in the database.
func (s SQLStorage) Create(t Todo) (Todo, error) {
	sql := `
		INSERT INTO todo(title, updated) 
		VALUES($1, NOW() )	
		RETURNING id	
	`

	if t.Complete {
		sql = `
		INSERT INTO todo(title, updated, completed) 
		VALUES($1,NOW(),NOW())
		RETURNING id	
	`
	}

	var id int

	if err := s.db.QueryRow(sql, t.Title).Scan(&id); err != nil {
		return t, fmt.Errorf("create error: on exec: %s", err)
	}

	t.ID = int(id)

	return t, nil
}

func resultToTodo(results *sql.Rows) (Todo, error) {
	t := Todo{}
	if err := results.Scan(&t.ID, &t.Title, &t.Updated, &t.completedNull); err != nil {
		return t, fmt.Errorf("resultToTodo error: on scan: %s", err)
	}

	if t.completedNull.Valid {
		t.Completed = t.completedNull.Time
		t.Complete = true
	}

	return t, nil
}

// Read returns a single todo from the database
func (s SQLStorage) Read(id string) (Todo, error) {
	t := Todo{}
	results, err := s.db.Query("SELECT * FROM todo WHERE id =$1;", id)
	if err != nil {
		s.log(fmt.Sprintf("could not read item: %s", err))
		return t, fmt.Errorf("read error: Query: %s", err)
	}

	results.Next()
	t, err = resultToTodo(results)
	if err != nil {
		return t, fmt.Errorf("read error: resultToTodo: %s", err)
	}

	return t, nil
}

// Update changes one todo in the database.
func (s SQLStorage) Update(t Todo) error {
	orig, err := s.Read(strconv.Itoa(t.ID))
	if err != nil {
		s.log(fmt.Sprintf("could not read item to update it: %s", err))
		return err
	}

	sql := `
		UPDATE todo
		SET title = $1, updated = NOW() 
		WHERE id = $2
	`

	if t.Complete && !orig.Complete {
		sql = `
		UPDATE todo
		SET title = $1, updated = NOW(), completed = NOW() 
		WHERE id = $2
	`
	}

	if orig.Complete && !t.Complete {
		sql = `
		UPDATE todo
		SET title = $1, updated = NOW(), completed = NULL 
		WHERE id = $2
	`
	}

	op, err := s.db.Prepare(sql)
	if err != nil {
		s.log(fmt.Sprintf("could not prepare item to update: %s", err))
		return fmt.Errorf("update error: on prepare: %s", err)
	}

	_, err = op.Exec(t.Title, t.ID)

	if err != nil {
		s.log(fmt.Sprintf("could not exec update: %s", err))
		return fmt.Errorf("update error: on exec: %s", err)
	}

	return nil
}

// Delete removes one todo from the database.
func (s SQLStorage) Delete(id string) error {
	op, err := s.db.Prepare("DELETE FROM todo WHERE id =$1")
	if err != nil {
		s.log(fmt.Sprintf("could not prepare item to delete: %s", err))
		return fmt.Errorf("delete error: on prepare: %s", err)
	}

	if _, err = op.Exec(id); err != nil {
		s.log(fmt.Sprintf("could not exec delete: %s", err))
		return fmt.Errorf("delete error: on exec: %s", err)
	}

	return nil
}

// RedisPool is an interface that allows us to swap in an mock for testing cache
// code.
type RedisPool interface {
	Get() redis.Conn
}

// ErrCacheMiss error indicates that an item is not in the cache
var ErrCacheMiss = fmt.Errorf("item is not in cache")

// NewCache returns an initialized cache ready to go.
func NewCache(redisHost, redisPort string, enabled bool) (*Cache, error) {
	c := &Cache{}

	if redisHost == "" {
		return nil, fmt.Errorf("redis host is blank")
	}

	if redisPort == "" {
		return nil, fmt.Errorf("redis port is blank")
	}

	pool := c.InitPool(redisHost, redisPort)
	c.enabled = enabled
	c.redisPool = pool
	return c, nil
}

// Cache abstracts all of the operations of caching for the application
type Cache struct {
	// redisPool *redis.Pool
	redisPool RedisPool
	enabled   bool
}

func (c *Cache) log(msg string) {
	log.Printf("Cache     : %s\n", msg)
}

// InitPool starts the cache off
func (c Cache) InitPool(redisHost, redisPort string) RedisPool {
	redisAddr := fmt.Sprintf("%s:%s", redisHost, redisPort)
	msg := fmt.Sprintf("Initialized Redis at %s", redisAddr)
	c.log(msg)
	const maxConnections = 10

	pool := redis.NewPool(func() (redis.Conn, error) {
		return redis.Dial("tcp", redisAddr)
	}, maxConnections)

	return pool
}

// Clear removes all items from the cache.
func (c Cache) Clear() error {
	if !c.enabled {
		return nil
	}
	conn := c.redisPool.Get()
	defer conn.Close()

	if _, err := conn.Do("FLUSHALL"); err != nil {
		return err
	}
	return nil
}

// Save records a todo into the cache.
func (c *Cache) Save(todo Todo) error {
	if !c.enabled {
		return nil
	}

	conn := c.redisPool.Get()
	defer conn.Close()

	json, err := todo.JSON()
	if err != nil {
		return err
	}

	conn.Send("MULTI")
	conn.Send("SET", strconv.Itoa(todo.ID), json)

	if _, err := conn.Do("EXEC"); err != nil {
		return err
	}
	c.log("Successfully saved todo to cache")
	return nil
}

// Get gets a todo from the cache.
func (c *Cache) Get(key string) (Todo, error) {
	t := Todo{}
	if !c.enabled {
		return t, ErrCacheMiss
	}
	conn := c.redisPool.Get()
	defer conn.Close()

	s, err := redis.String(conn.Do("GET", key))
	if err == redis.ErrNil {
		return Todo{}, ErrCacheMiss
	} else if err != nil {
		return Todo{}, err
	}

	if err := json.Unmarshal([]byte(s), &t); err != nil {
		return Todo{}, err
	}
	c.log("Successfully retrieved todo from cache")

	return t, nil
}

// Delete will remove a todo from the cache completely.
func (c *Cache) Delete(key string) error {
	if !c.enabled {
		return nil
	}
	conn := c.redisPool.Get()
	defer conn.Close()

	if _, err := conn.Do("DEL", key); err != nil {
		return err
	}

	c.log(fmt.Sprintf("Cleaning from cache %s", key))
	return nil
}

// List gets all of the todos from the cache.
func (c *Cache) List() (Todos, error) {
	t := Todos{}
	if !c.enabled {
		return t, ErrCacheMiss
	}
	conn := c.redisPool.Get()
	defer conn.Close()

	s, err := redis.String(conn.Do("GET", "todoslist"))
	if err == redis.ErrNil {
		return Todos{}, ErrCacheMiss
	} else if err != nil {
		return Todos{}, err
	}

	if err := json.Unmarshal([]byte(s), &t); err != nil {
		return Todos{}, err
	}
	c.log("Successfully retrieved todos from cache")

	return t, nil
}

// SaveList records a todo list into the cache.
func (c *Cache) SaveList(todos Todos) error {
	if !c.enabled {
		return nil
	}

	conn := c.redisPool.Get()
	defer conn.Close()

	json, err := todos.JSON()
	if err != nil {
		return err
	}

	if _, err := conn.Do("SET", "todoslist", json); err != nil {
		return err
	}
	c.log("Successfully saved todo to cache")
	return nil
}

// DeleteList deletes a todo list into the cache.
func (c *Cache) DeleteList() error {
	if !c.enabled {
		return nil
	}

	return c.Delete("todoslist")
}

type Todo struct {
	ID            int       `json:"id"`
	Title         string    `json:"title"`
	Updated       time.Time `json:"updated"`
	Completed     time.Time `json:"completed"`
	Complete      bool      `json:"complete"`
	completedNull sql.NullTime
}

type Storage struct {
	sqlstorage SQLStorage
	cache      *Cache
}

// Init kicks off the database connector
func (s *Storage) Init(user, password, host, name, conn, redisHost, redisPort string, cache bool) error {
	if err := s.sqlstorage.Init(user, password, host, name, conn); err != nil {
		return err
	}

	var err error
	s.cache, err = NewCache(redisHost, redisPort, cache)
	if err != nil {
		return err
	}

	return nil
}

func (s Storage) List() (Todos, error) {
	ts, err := s.cache.List()
	if err != nil {
		if err == ErrCacheMiss {
			ts, err = s.sqlstorage.List()
			if err != nil {
				return ts, fmt.Errorf("error getting todo: %v", err)
			}
		}
		if err := s.cache.SaveList(ts); err != nil {
			return ts, fmt.Errorf("error caching todo : %v", err)
		}
	}

	return ts, nil
}

// Create records a new todo in the database.
func (s Storage) Create(t Todo) (Todo, error) {
	if err := s.cache.DeleteList(); err != nil {
		return Todo{}, fmt.Errorf("error clearing cache : %v", err)
	}

	t, err := s.sqlstorage.Create(t)
	if err != nil {
		return t, err
	}

	if err = s.cache.Save(t); err != nil {
		return t, err
	}

	return t, nil
}

// Read returns a single todo from cache or database
func (s Storage) Read(id string) (Todo, error) {
	t, err := s.cache.Get(id)
	if err != nil {
		if err == ErrCacheMiss {
			t, err = s.sqlstorage.Read(id)
			if err != nil {
				return t, fmt.Errorf("error getting todo: %v", err)
			}
		}
		if err := s.cache.Save(t); err != nil {
			return t, fmt.Errorf("error caching todo : %v", err)
		}
	}

	return t, nil
}

// Update changes one todo in the database.
func (s Storage) Update(t Todo) error {
	if err := s.cache.DeleteList(); err != nil {
		return fmt.Errorf("error clearing cache : %v", err)
	}

	if err := s.sqlstorage.Update(t); err != nil {
		return err
	}

	if err := s.cache.Save(t); err != nil {
		return err
	}

	return nil
}

// Delete removes one todo from the database.
func (s Storage) Delete(id string) error {
	if err := s.cache.DeleteList(); err != nil {
		return fmt.Errorf("error clearing cache : %v", err)
	}

	if err := s.sqlstorage.Delete(id); err != nil {
		return err
	}

	if err := s.cache.Delete(id); err != nil {
		return err
	}

	return nil
}

// JSON marshalls the content of a todo to json.
func (t Todo) JSON() (string, error) {
	bytes, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("could not marshal json for response: %s", err)
	}

	return string(bytes), nil
}

// JSONBytes marshalls the content of a todo to json as a byte array.
func (t Todo) JSONBytes() ([]byte, error) {
	bytes, err := json.Marshal(t)
	if err != nil {
		return []byte{}, fmt.Errorf("could not marshal json for response: %s", err)
	}

	return bytes, nil
}

// Key returns the id as a string.
func (t Todo) Key() string {
	return strconv.Itoa(t.ID)
}

type Todos []Todo

// JSON marshalls the content of a slice of todos to json.
func (t Todos) JSON() (string, error) {
	bytes, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("could not marshal json for response: %s", err)
	}

	return string(bytes), nil
}

// JSONBytes marshalls the content of a slice of todos to json as a byte array.
func (t Todos) JSONBytes() ([]byte, error) {
	bytes, err := json.Marshal(t)
	if err != nil {
		return []byte{}, fmt.Errorf("could not marshal json for response: %s", err)
	}

	return bytes, nil
}

var (
	storage    Storage
	signalChan chan (os.Signal) = make(chan os.Signal, 1)
)

func main() {
	conn := os.Getenv("CLOUD_SQL_DATABASE_CONNECTION_NAME")
	user := os.Getenv("CLOUD_RUN_SERVICE_ACCOUNT")
	host := os.Getenv("CLOUD_SQL_DATABASE_HOST")
	name := os.Getenv("CLOUD_SQL_DATABASE_NAME")
	pass := os.Getenv("db_pass")
	redisHost := os.Getenv("REDIS_HOST")
	redisPort := os.Getenv("REDIS_PORT")
	port := os.Getenv("PORT")

	if err := storage.Init(user, pass, host, name, conn, redisHost, redisPort, true); err != nil {
		log.Fatalf("cannot initialize storage systems: %s", err)
	}
	defer storage.sqlstorage.Close()

	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	headersOk := handlers.AllowedHeaders([]string{"X-Requested-With"})
	originsOk := handlers.AllowedOrigins([]string{"*"})
	methodsOk := handlers.AllowedMethods([]string{"GET", "HEAD", "POST", "PUT", "OPTIONS", "DELETE"})

	router := mux.NewRouter().StrictSlash(true)
	router.HandleFunc("/healthz", healthHandler).Methods(http.MethodGet)
	router.HandleFunc("/api/v1/todo", listHandler).Methods(http.MethodGet, http.MethodOptions)
	router.HandleFunc("/api/v1/todo", createHandler).Methods(http.MethodPost)
	router.HandleFunc("/api/v1/todo/{id}", readHandler).Methods(http.MethodGet)
	router.HandleFunc("/api/v1/todo/{id}", deleteHandler).Methods(http.MethodDelete)
	router.HandleFunc("/api/v1/todo/{id}", updateHandler).Methods(http.MethodPost, http.MethodPut)

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: httpLog(handlers.CORS(originsOk, headersOk, methodsOk)(router)),
	}

	go func() {
		log.Printf("listening on port %s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	sig := <-signalChan
	log.Printf("%s signal caught", sig)

	// Timeout if waiting for connections to return idle.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	storage.sqlstorage.Close()

	// Gracefully shutdown the server by waiting on existing requests (except websockets).
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("server shutdown failed: %+v", err)
	}
	log.Print("server exited")
}

// CORSRouterDecorator applies CORS headers to a mux.Router
type CORSRouterDecorator struct {
	R *mux.Router
}

// ServeHTTP wraps the HTTP server enabling CORS headers.
// For more info about CORS, visit https://www.w3.org/TR/cors/
func (c *CORSRouterDecorator) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if origin := req.Header.Get("Origin"); origin != "" {
		rw.Header().Set("Access-Control-Allow-Origin", "*")
		rw.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
		rw.Header().Set("Access-Control-Allow-Headers", "Accept, Accept-Language, Content-Type, YourOwnHeader")
	}
	// Stop here if its Preflighted OPTIONS request
	if req.Method == "OPTIONS" {
		return
	}

	c.R.ServeHTTP(rw, req)
}

func httpLog(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := "%s %s %s"
		logS := fmt.Sprintf(s, r.RemoteAddr, r.Method, r.RequestURI)
		weblog(logS)
		h.ServeHTTP(w, r) // call original
	})
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
	return
}

func listHandler(w http.ResponseWriter, r *http.Request) {
	ts, err := storage.List()
	if err != nil {
		writeErrorMsg(w, err)
		return
	}

	writeJSON(w, ts, http.StatusOK)
}

func readHandler(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]

	_, err := strconv.Atoi(id)
	if err != nil {
		msg := Message{"invalid! id must be integer", fmt.Sprintf("todo id: %s", id)}
		writeJSON(w, msg, http.StatusInternalServerError)
		return
	}

	t, err := storage.Read(id)
	if err != nil {

		if strings.Contains(err.Error(), "Rows are closed") {
			msg := Message{"todo not found", fmt.Sprintf("todo id: %s", id)}
			writeJSON(w, msg, http.StatusNotFound)
			return
		}

		writeErrorMsg(w, err)
		return
	}

	writeJSON(w, t, http.StatusOK)
}

func createHandler(w http.ResponseWriter, r *http.Request) {
	t := Todo{}
	t.Title = r.FormValue("title")

	if len(r.FormValue("complete")) > 0 && r.FormValue("complete") != "false" {
		t.Complete = true
	}

	t, err := storage.Create(t)
	if err != nil {
		writeErrorMsg(w, err)
		return
	}

	writeJSON(w, t, http.StatusCreated)
}

func updateHandler(w http.ResponseWriter, r *http.Request) {
	var err error
	t := Todo{}
	id := mux.Vars(r)["id"]
	t.ID, err = strconv.Atoi(id)
	if err != nil {
		writeErrorMsg(w, err)
		return
	}

	t.Title = r.FormValue("title")

	if len(r.FormValue("complete")) > 0 && r.FormValue("complete") != "false" {
		t.Complete = true
	}

	if err = storage.Update(t); err != nil {
		writeErrorMsg(w, err)
		return
	}

	writeJSON(w, t, http.StatusOK)
}

func deleteHandler(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]

	_, err := strconv.Atoi(id)
	if err != nil {
		msg := Message{"invalid! id must be integer", fmt.Sprintf("todo id: %s", id)}
		writeJSON(w, msg, http.StatusInternalServerError)
		return
	}

	if err := storage.Delete(id); err != nil {
		writeErrorMsg(w, err)
		return
	}
	msg := Message{"todo deleted", fmt.Sprintf("todo id: %s", id)}

	writeJSON(w, msg, http.StatusNoContent)
}

// JSONProducer is an interface that spits out a JSON string version of itself
type JSONProducer interface {
	JSON() (string, error)
	JSONBytes() ([]byte, error)
}

func writeJSON(w http.ResponseWriter, j JSONProducer, status int) {
	json, err := j.JSON()
	if err != nil {
		writeErrorMsg(w, err)
		return
	}
	writeResponse(w, status, json)
	return
}

func writeErrorMsg(w http.ResponseWriter, err error) {
	s := fmt.Sprintf("{\"error\":\"%s\"}", err)
	writeResponse(w, http.StatusInternalServerError, s)
	return
}

func writeResponse(w http.ResponseWriter, status int, msg string) {
	if status != http.StatusOK {
		weblog(fmt.Sprintf(msg))
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type,access-control-allow-origin, access-control-allow-headers")
	w.WriteHeader(status)
	w.Write([]byte(msg))

	return
}

func weblog(msg string) {
	log.Printf("Webserver : %s", msg)
}

// Message is a structure for communicating additional data to API consumer.
type Message struct {
	Text    string `json:"text"`
	Details string `json:"details"`
}

// JSON marshalls the content of a todo to json.
func (m Message) JSON() (string, error) {
	bytes, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("could not marshal json for response: %s", err)
	}

	return string(bytes), nil
}

// JSONBytes marshalls the content of a todo to json as a byte array.
func (m Message) JSONBytes() ([]byte, error) {
	bytes, err := json.Marshal(m)
	if err != nil {
		return []byte{}, fmt.Errorf("could not marshal json for response: %s", err)
	}

	return bytes, nil
}
