package main

import (
	"code.google.com/p/go.net/websocket"
	"database/sql"
	"fmt"
	_ "github.com/lib/pq"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
)

func main() {
	db, err := sql.Open("postgres", "sslmode=disable")
	if err != nil {
		log.Println(err.Error())
		return
	}
	_ = db

	ds := NewFsDatastore("./data")
	if err := ds.Open(); err != nil {
		log.Println(err)
		return
	}

	for i := 0; i < 250; i++ {
		//	srv := NewServer(NewSqlDatastore(db, 20))
		srv := NewServer(ds, "srv"+strconv.Itoa(i)+"/")

		if i == 0 {
			go func() {
				httpSrv := http.Server{
					Addr:    ":6000",
					Handler: srv.(*server),
				}
				httpSrv.ListenAndServe()
			}()
		}

		err = srv.Start()
		if err != nil {
			log.Println(err.Error())
			return
		}

		inj := UDPInjector{Addr: ":" + strconv.Itoa(6000+i), Server: srv}
		inj.Start()
	}
	<-make(chan int)
}

func (srv *server) ServeHTTP(rw http.ResponseWriter, rq *http.Request) {
	path := rq.URL.Path
	if len(path) == 0 || path[0] != '/' {
		return
	}

	if len(path) >= 8 && path[0:8] == "/static/" {
		http.ServeFile(rw, rq, "./static/"+path[8:])
	} else if len(path) >= 6 && path[0:6] == "/live:" {
		x := strings.Split(path[6:], ":")
		if rq.Header.Get("Upgrade") == "websocket" {
			w, err := srv.LiveWatch(x[0], x[1:])
			if err != nil {
				ohCrap(rw, err)
				return
			}
			websocket.Handler(func(conn *websocket.Conn) {
				fmt.Fprint(conn, w.Ts)
				for v := range w.C {
					if err := printSlice(conn, v); err != nil {
						w.Close()
					}
				}
			}).ServeHTTP(rw, rq)
		} else {
			data, ts, err := srv.LiveLog(x[0], x[1:])
			if err != nil {
				ohCrap(rw, err)
				return
			}
			fmt.Fprint(rw, "[", ts, ",")
			for i, row := range data {
				if i != 0 {
					fmt.Fprint(rw, ",")
				}
				printSlice(rw, row)
			}
			fmt.Fprint(rw, "]")
		}
	} else {
		x := strings.Split(path[1:], ":")
		if rq.Header.Get("Upgrade") == "websocket" {
			offs, err := param(rw, rq, "offs")
			if err != nil {
				return
			}
			gran, err := param(rw, rq, "gran")
			if err != nil {
				return
			}
			w, err := srv.Watch(x[0], x[1:], offs, gran)
			if err != nil {
				ohCrap(rw, err)
				return
			}
			websocket.Handler(func(conn *websocket.Conn) {
				fmt.Fprint(conn, w.Ts)
				for v := range w.C {
					if err := printSlice(conn, v); err != nil {
						w.Close()
					}
				}
			}).ServeHTTP(rw, rq)
		} else {
			from, err := param(rw, rq, "from")
			if err != nil {
				return
			}
			length, err := param(rw, rq, "length")
			if err != nil {
				return
			}
			gran, err := param(rw, rq, "gran")
			if err != nil {
				return
			}

			data, err := srv.Log(x[0], x[1:], from, length, gran)
			if err != nil {
				ohCrap(rw, err)
				return
			}
			fmt.Fprint(rw, "[", from, ",")
			for i, row := range data {
				if i != 0 {
					fmt.Fprint(rw, ",")
				}
				printSlice(rw, row)
			}
			fmt.Fprint(rw, "]")
		}
	}
}

func ohCrap(rw http.ResponseWriter, err error) {
	rw.WriteHeader(http.StatusBadRequest)
	rw.Write([]byte(err.Error()))
}

func param(rw http.ResponseWriter, rq *http.Request, name string) (int64, error) {
	val, err := strconv.ParseInt(rq.URL.Query().Get(name), 10, 64)
	if err != nil {
		ohCrap(rw, err)
	}
	return val, err
}

func printSlice(w io.Writer, data []float64) error {
	r := "["
	for j, val := range data {
		if j != 0 {
			r += ","
		}
		r += strconv.FormatFloat(val, 'e', -1, 64)
	}
	r += "]"
	_, err := fmt.Fprint(w, r)
	return err
}
