package main

import (
	"fmt"
	"log"

	"example.com/realistic/server"
	"example.com/realistic/worker"
)

func main() {
	cfg := server.Config{Host: "localhost", Port: 8080}
	srv, err := server.New(cfg)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(srv.Addr())
	fmt.Println(server.Tags("env", "prod", "region", "us-east-1"))

	w := worker.New()
	w.Run(nil, worker.Task{ID: 1, Payload: "hello"})

	combined := worker.Combine(worker.Result{TaskID: 1, Output: "a"}, worker.Result{TaskID: 2, Output: "b"})
	fmt.Println(combined.Output)
}
