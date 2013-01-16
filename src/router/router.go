/*

For keeping a minimum running, perhaps when doing a routing table update, if destination hosts are all
 expired or about to expire we start more. 

*/

package main

import (
	"encoding/json"
	"fmt"
	"github.com/gorilla/mux"
	"github.com/iron-io/iron_go/cache"
	"github.com/iron-io/iron_go/worker"
	"github.com/iron-io/common"
	"log"
	"math/rand"
	// "net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"
	"runtime"
	"flag"
//	"io/ioutil"
)

var config struct {
	Iron struct {
	Token      string `json:"token"`
	ProjectId  string `json:"project_id"`
} `json:"iron"`
	Logging       struct {
	To     string `json:"to"`
	Level  string `json:"level"`
	Prefix string `json:"prefix"`
}
}

//var routingTable = map[string]*Route{}
var icache = cache.New("routing-table")

func init() {

}

type Route struct {
	// TODO: Change destinations to a simple cache so it can expire entries after 55 minutes (the one we use in common?)
	Host         string `json:"host"`
	Destinations []string  `json:"destinations"`
	ProjectId    string  `json:"project_id"`
	Token        string  `json:"token"` // store this so we can queue up new workers on demand
	CodeName     string  `json:"code_name"`
}

// for adding new hosts
type Route2 struct {
	Host string `json:"host"`
	Dest string `json:"dest"`
}

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	log.Println("Running on", runtime.NumCPU(), "CPUs")

	var configFile string
	var env string
	flag.StringVar(&configFile, "c", "", "Config file name")
	// when this was e, it was erroring out.
	flag.StringVar(&env, "e2", "development", "environment")

	flag.Parse() // Scans the arg list and sets up flags

	// Deployer is now passing -c in since we're using upstart and it doesn't know what directory to run in
	if configFile == "" {
		configFile = "config_" + env + ".json"
	}

	common.LoadConfig("iron_mq", configFile, &config)
	common.SetLogLevel(config.Logging.Level)
	common.SetLogLocation(config.Logging.To, config.Logging.Prefix)

	icache.Settings.UseConfigMap(map[string]interface{}{"token": config.Iron.Token, "project_id": config.Iron.ProjectId})

	r := mux.NewRouter()
	s := r.Headers("Iron-Router", "").Subrouter()
	s.HandleFunc("/", AddWorker)
	r.HandleFunc("/addworker", AddWorker)

	r.HandleFunc("/", ProxyFunc)

	http.Handle("/", r)
	port := 80
	fmt.Println("listening and serving on port", port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%v", port), nil))
}

func ProxyFunc(w http.ResponseWriter, req *http.Request) {
	fmt.Println("HOST:", req.Host)
	host := strings.Split(req.Host, ":")[0]

	// We look up the destinations in the routing table and there can be 3 possible scenarios:
	// 1) This host was never registered so we return 404
	// 2) This host has active workers so we do the proxy
	// 3) This host has no active workers so we queue one (or more) up and return a 503 or something with message that says "try again in a minute"
	//	route := routingTable[host]
 fmt.Println("getting route for host:", host)
	route, err := getRoute(host)
	// choose random dest
	if err != nil {
		common.SendError(w, 400, fmt.Sprintln("Host not registered or error!", err))
		return
	}
	//	if route == nil { // route.Host == "" {
	//		common.SendError(w, 400, fmt.Sprintln(w, "Host not configured!"))
	//		return
	//	}
	dlen := len(route.Destinations)
	if dlen == 0 {
		fmt.Println("No workers running, starting new task.")
		startNewWorker(route)
		common.SendError(w, 500, fmt.Sprintln("No workers running, starting them up..."))
		return
	}
	if dlen == 1 {
		fmt.Println("Only one worker running, starting a new task.")
		startNewWorker(route)
	}
	destIndex := rand.Intn(dlen)
	destUrlString := route.Destinations[destIndex]
	// todo: should check if http:// already exists.
	destUrlString2 := "http://" + destUrlString
	destUrl, err := url.Parse(destUrlString2)
	if err != nil {
		fmt.Println("error!", err)
		panic(err)
	}
	fmt.Println("proxying to", destUrl)
	proxy := NewSingleHostReverseProxy(destUrl)
	err = proxy.ServeHTTP(w, req)
	if err != nil {
		fmt.Println("Error proxying!", err)
		etype := reflect.TypeOf(err)
		fmt.Println("err type:", etype)
		w.WriteHeader(http.StatusInternalServerError)
		// can't figure out how to compare types so comparing strings.... lame. 
		if strings.Contains(etype.String(), "net.OpError") { // == reflect.TypeOf(net.OpError{}) { // couldn't figure out a better way to do this
			if len(route.Destinations) > 2 { // always want at least two running
				fmt.Println("It's a network error, removing this destination from routing table.")
				route.Destinations = append(route.Destinations[:destIndex], route.Destinations[destIndex + 1:]...)
				err := putRoute(route)
				if err != nil {
					fmt.Println("couldn't update routing table 1", err)
					common.SendError(w, 500, fmt.Sprintln("couldn't update routing table 1", err))
					return
				}
				fmt.Println("New route:", route)
				return
			} else {
				fmt.Println("It's a network error and less than two other workers available so we're going to remove it and start new task.")
				route.Destinations = append(route.Destinations[:destIndex], route.Destinations[destIndex + 1:]...)
				err := putRoute(route)
				if err != nil {
					fmt.Println("couldn't update routing table:", err)
					common.SendError(w, 500, fmt.Sprintln("couldn't update routing table", err))
					return
				}
				fmt.Println("New route:", route)
			}
			// start new worker if it's a connection error
			startNewWorker(route)
		}
		return
	}
	fmt.Println("Served!")
	// todo: how to handle destination failures. I got this in log output when testing a bad endpoint:
	// 2012/12/26 23:22:08 http: proxy error: dial tcp 127.0.0.1:8082: connection refused
}

func startNewWorker(route *Route) (error) {
	fmt.Println("Starting a new worker")
	// start new worker
	payload := map[string]interface{}{
		"token":      route.Token,
		"project_id": route.ProjectId,
		"code_name":  route.CodeName,
	}
	workerapi := worker.New()
	workerapi.Settings.UseConfigMap(payload)
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		fmt.Println("Couldn't marshal json!", err)
		return err
	}
	timeout := time.Second * time.Duration(1800 + rand.Intn(600)) // a little random factor in here to spread out worker deaths
	task := worker.Task{
		CodeName: route.CodeName,
		Payload:  string(jsonPayload),
		Timeout:  &timeout, // let's have these die quickly while testing
	}
	tasks := make([]worker.Task, 1)
	tasks[0] = task
	taskIds, err := workerapi.TaskQueue(tasks...)
	fmt.Println("Tasks queued.", taskIds)
	if err != nil {
		fmt.Println("Couldn't queue up worker!", err)
		return err
	}
	return err
}


// When a worker starts up, it calls this
func AddWorker(w http.ResponseWriter, req *http.Request) {
	log.Println("AddWorker called!")

//	s, err := ioutil.ReadAll(req.Body)
//	fmt.Println("req.body:", err, string(s))

	// get project id and token
	projectId := req.FormValue("project_id")
	token := req.FormValue("token")
	codeName := req.FormValue("code_name")
	fmt.Println("project_id:", projectId, "token:", token, "code_name:", codeName)

	// check header for what operation to perform
	routerHeader := req.Header.Get("Iron-Router")
	if routerHeader == "register" {
		route := Route{}
		if !common.ReadJSON(w, req, &route) {
			return
		}
		fmt.Println("body read into route:", route)
		route.ProjectId = projectId
		route.Token = token
		route.CodeName = codeName
		// todo: do we need to close body?
		err := putRoute(&route)
		if err != nil {
			fmt.Println("couldn't register host:", err)
			common.SendError(w, 400, fmt.Sprintln("Could not register host!", err))
			return
		}
		fmt.Println("registered route:", route)
		fmt.Fprintln(w, "Host registered successfully.")

	} else {
		r2 := Route2{}
		if !common.ReadJSON(w, req, &r2) {
			return
		}
		// todo: do we need to close body?
		fmt.Println("DECODED:", r2)
		route, err := getRoute(r2.Host)
		//		route := routingTable[r2.Host]
		if err != nil {
			common.SendError(w, 400, fmt.Sprintln("This host is not registered!", err))
			return
			//			route = &Route{}
		}
		fmt.Println("ROUTE:", route)
		route.Destinations = append(route.Destinations, r2.Dest)
		fmt.Println("ROUTE new:", route)
		err = putRoute(route)
		if err != nil {
			fmt.Println("couldn't register host:", err)
			common.SendError(w, 400, fmt.Sprintln("Could not register host!", err))
			return
		}
		//		routingTable[r2.Host] = route
		//		fmt.Println("New routing table:", routingTable)
		fmt.Fprintln(w, "Worker added")
	}
}

func getRoute(host string) (*Route, error) {
	rx, err := icache.Get(host)
	if err != nil {
		return nil, err
	}
	rx2 := []byte(rx.(string))
	route := Route{}
	err = json.Unmarshal(rx2, &route)
	if err != nil {
		return nil, err
	}
	return &route, err
}

func putRoute(route *Route) (error) {
	item := cache.Item{}
	v, err := json.Marshal(route)
	if err != nil {
		return err
	}
	item.Value = string(v)
	err = icache.Put(route.Host, &item)
	return err
}
