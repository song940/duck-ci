package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
)

var (
	port string
)

func main() {

	flag.StringVar(&port, "port", "4000", "http port")
	flag.Parse()

	config := DuckCIConfig{
		Database: "duckci.db",
	}
	ci, err := New(config)
	if err != nil {
		log.Fatal(err)
	}
	http.HandleFunc("/", ci.IndexView)
	http.HandleFunc("/new", ci.ProjectView)
	http.HandleFunc("/projects", ci.ProjectView)
	http.HandleFunc("/task", ci.TaskView)
	http.ListenAndServe(fmt.Sprintf(":%s", port), nil)
}
