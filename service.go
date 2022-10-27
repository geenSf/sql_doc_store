/*
 * Copyright 2020 Matthew A. Titmus
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"

	"io"
	"log"
	"net/http"
	"net/url"
	"os"

	middleware "main/internal/middleware"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"golang.org/x/net/html/charset"
)

var transact TransactionLogger

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		next.ServeHTTP(w, r)
	})
}

func notAllowedHandler(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "Not Allowed", http.StatusMethodNotAllowed)
}

func keyValuePutHandler(w http.ResponseWriter, r *http.Request) {

	vars := mux.Vars(r)

	key := vars["key"]

	// преобразование в utf8
	utf8, err := charset.NewReader(r.Body, r.Header.Get("Content-Type"))

	defer r.Body.Close()

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	value, err := io.ReadAll(utf8)
	if err != nil {
		fmt.Println("IO error:", err)
		return
	}

	valueModif := string(value)
	valueModif, _ = url.QueryUnescape(valueModif)

	err = Put(key, string([]rune(valueModif)))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	transact.WritePut(key, string([]rune(valueModif)))
	w.WriteHeader(http.StatusCreated)

	log.Printf("PUT key=%s value=%s\n", key, string(string([]rune(valueModif))))
}

func keyValueGetHandler(w http.ResponseWriter, r *http.Request) {

	vars := mux.Vars(r)
	key := vars["key"]

	value, err := Get(key)
	if errors.Is(err, ErrorNoSuchKey) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Write([]byte(value))

	log.Printf("GET key=%s\n", key)
}

func collectionGetHandler(w http.ResponseWriter, r *http.Request) {

	store, err := GetCollection()

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	//var writer io.Writer
	var buf bytes.Buffer
	buf.WriteByte('\n')
	buf.WriteString("List of documents:\n")
	for k, _ := range store {
		buf.WriteString(k)
		//		buf.WriteByte(':')
		//		buf.WriteString(v)
		buf.WriteByte('\n')
	}

	w.Header().Set("Content-Type", "application/json")

	w.Write(buf.Bytes())

	log.Printf("GET collection\n")
}

func keyValueDeleteHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	key := vars["key"]

	err := Delete(key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	transact.WriteDelete(key)

	log.Printf("DELETE key=%s\n", key)
}

func initializeTransactionLog() error {
	var err error

	transact, err = NewPostgresTransactionLogger(PostgresDbParams{
		host:     "localhost",
		dbName:   "kvs",
		user:     "test",
		password: "hunter2",
	})
	if err != nil {
		return fmt.Errorf("failed to create transaction logger: %w", err)
	}

	events, errors := transact.ReadEvents()
	count, ok, e := 0, true, Event{}

	for ok && err == nil {
		select {
		case err, ok = <-errors:

		case e, ok = <-events:
			switch e.EventType {
			case EventDelete: // Got a DELETE event!
				err = Delete(e.Key)
				count++
			case EventPut: // Got a PUT event!
				err = Put(e.Key, e.Value)
				count++
			}
		}
	}

	log.Printf("%d events replayed\n", count)

	transact.Run()

	go func() {
		for err := range transact.Err() {
			log.Print(err)
		}
	}()

	return err
}

func main() {

	//********************************
	//установка номера порта
	//потом перенести в модуль init
	os.Setenv("SERVERPORT", "4000")
	//********************************

	certFile := flag.String("certfile", "cert.pem", "certificate PEM file")
	keyFile := flag.String("keyfile", "key.pem", "key PEM file")
	flag.Parse()

	// Create a new mux router
	r := mux.NewRouter()
	r.StrictSlash(true)
	//server := NewTaskServer()

	// Initializes the transaction log and loads existing data, if any.
	// Blocks until all data is read.
	err := initializeTransactionLog()
	if err != nil {
		panic(err)
	}

	r.Use(loggingMiddleware)

	r.Handle("/docs/v1/{key}", middleware.BasicAuth(http.HandlerFunc(keyValuePutHandler))).Methods("POST")
	r.Handle("/docs/v1/{key}", middleware.BasicAuth(http.HandlerFunc(keyValuePutHandler))).Methods("PUT")

	r.HandleFunc("/docs/v1/{key}", keyValueGetHandler).Methods("GET")
	//	r.HandleFunc("/docs/{key}", keyValuePutHandler).Methods("PUT")
	//	r.HandleFunc("/docs/{key}", keyValuePutHandler).Methods("POST")
	r.HandleFunc("/docs/v1/{key}", keyValueDeleteHandler).Methods("DELETE")
	r.HandleFunc("/docs/v1/", collectionGetHandler).Methods("GET")

	//r.HandleFunc("/docs/v1/", notAllowedHandler)
	r.HandleFunc("/docs/v1/{key}", notAllowedHandler)

	// Set up logging and panic recovery middleware for all paths.
	r.Use(func(h http.Handler) http.Handler {
		return handlers.LoggingHandler(os.Stdout, h)
	})
	r.Use(handlers.RecoveryHandler(handlers.PrintRecoveryStack(true)))

	addr := "localhost:" + os.Getenv("SERVERPORT")
	srv := &http.Server{
		Addr:    addr,
		Handler: r,
		TLSConfig: &tls.Config{
			MinVersion:               tls.VersionTLS13,
			PreferServerCipherSuites: true,
		},
	}

	log.Printf("Starting server on %s", addr)
	log.Fatal(srv.ListenAndServeTLS(*certFile, *keyFile))

}
