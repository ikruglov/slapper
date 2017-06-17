package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
)

func main() {
	http.HandleFunc("/test", func(w http.ResponseWriter, r *http.Request) {
		ioutil.ReadAll(r.Body)
		r.Body.Close()

		sum := 0
		for rnd := rand.Intn(10000000); rnd > 0; rnd-- {
			sum += rnd
		}

		fmt.Fprintf(w, "Hello, %d", sum)
	})

	log.Fatal(http.ListenAndServe(":5000", nil))
}
