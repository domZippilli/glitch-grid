package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
)

// A control server which maintains a list of vaults which will store the data.
type ControlServer struct {
	mux      *http.ServeMux
	Vaults   []string
	minValue int
	lock     sync.RWMutex
}

// Create and return a new Control server instance.
// Provide a comma-separated list of vaults with which we will communicate.
func NewControlServer(vaults string) *ControlServer {

	s := new(ControlServer)
	s.mux = http.NewServeMux()
	s.Vaults = strings.Split(vaults, ",")
	s.minValue = 0
	s.lock = sync.RWMutex{}
	s.mux.HandleFunc("/", s.handle)
	// Set the default timeout for all HTTP operations to be one second.
	http.DefaultClient.Timeout = time.Second
	glog.Infof("Defined %d vaults", len(s.Vaults))
	return s
}

// Handle GET and POST requests to the root path.
func (s *ControlServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		// We only support operations on the root path.
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		s.get(w, r)
	} else if r.Method == http.MethodPost {
		s.post(w, r)
	} else {
		// Do not support PATCH, DELETE, etc, operations.
		http.NotFound(w, r)
	}
}

// Get the current value of the counter.
// Poll all our backend servers and see if we have majority consensus.
// Sends a 200 and the value to the client if we have a consensus, 500 otherwise.
func (s *ControlServer) get(w http.ResponseWriter, r *http.Request) {
	result := s.getValueFromVaults()
	var statusCode int
	var body string
	if result >= 0 {
		statusCode = http.StatusOK
		body = fmt.Sprintf("%d", result)
	} else {
		statusCode = http.StatusInternalServerError
		body = "-1"
	}
	w.WriteHeader(statusCode)
	w.Write([]byte(body))
}

// Get the consensus value stored across our vaults.
// Talk to each vault and get the value stored in said vault. If a majority of the vaults have the same
// value, then we have consensus and can return that value. If there is no consensus, return -1.
func (s *ControlServer) getValueFromVaults() int {
	var wg sync.WaitGroup
	m := sync.RWMutex{}
	// Map from a value to the number of vaults which currently have that value.
	counts := map[int]int{}
	// Loop over all the vault addresses, and execute each one in a separate goroutine.
	// Use a WaitGroup to keep track of the pending functions, and a ReadWrite lock to
	// protect access to the counts tracker.
	for _, vault := range s.Vaults {
		wg.Add(1)
		go func(m *sync.RWMutex, vault string, counts map[int]int) {
			defer wg.Done()
			getValueFromVault(m, vault, counts)
		}(&m, vault, counts)
	}
	wg.Wait()
	glog.Infof("Counts data: %v", counts)
	if len(counts) == 0 {
		glog.Error("Could not reach any vaults to get counts data")
		return -1
	}
	// Iterate over the map of values to the count of vaults with that value.
	// If any count represents a majority, then by default it will have the maximum
	// number of vaults associated with it. Otherwise, just keep track the maximum
	// number of counts associated with any value.
	// E.g., if we have seven vaults, and:
	// - vaults (A, C, G) have value "1";
	// - vaults (B, D, E) have value "2"; and
	// - vault F has value "4"
	// then the maximum number of vaults with the same value is three (the first two groups),
	// but is not enough to achieve consensus.
	maxVal := 0
	for v, c := range counts {
		if c > maxVal {
			maxVal = c
		}
		if s.hasMajority(c) {
			// We have consensus. Return the value.
			return v
		}
	}
	// We do not have consensus, but we do know how popular the most common value(s) is/are.
	glog.Warningf("No majority; only have %d/%d with a consensus value", maxVal, len(s.Vaults))
	return -1
}

// Get the value stored in a single vault.
// If we are able to fetch a valid integer from the vault, update the counts map with that
// information in a thread-safe way. Otherwise, return without updating (but log the issue).
func getValueFromVault(m *sync.RWMutex, vault string, counts map[int]int) {
	url := fmt.Sprintf("http://%s/", vault)
	var resp *http.Response
	var err error
	if resp, err = http.Get(url); err != nil {
		// This could include a timeout.
		glog.Warningf("Error getting value from vault %s: %v\n", url, err)
		return
	}
	if resp.StatusCode != http.StatusOK {
		// Vault was not happy.
		glog.Warningf("Error getting value from vault %s: invalid status code %v\n", url, resp.StatusCode)
		return
	}
	body, readError := ioutil.ReadAll(resp.Body)
	if readError != nil {
		// Vault was supposedly-happy but did not return a value.
		glog.Warningf("Error getting value from vault %s: error reading from body: %v\n", url, readError)
		return
	}
	v, e := strconv.Atoi(string(body))
	if e != nil {
		// Vault returned a value, but it was not a valid integer.
		glog.Warningf("Error getting value from vault %s: invalid body response: %v (%v)\n", url, body, e)
		return
	}
	// If we've gotten here, then we received a valid integer back from the vault.
	// Start the map manipulation operation critical section.
	m.Lock()
	count, ok := counts[v]
	if !ok {
		// This value is not (yet) in the map. IOW, there are currently 0 vaults storing that value.
		count = 0
	}
	counts[v] = count + 1
	m.Unlock()
	// End of the map manipulation critical section.
	glog.V(1).Infof("Get vault %s Value %d", url, v)
}

// Update the value in storage to what is provided in the body.
// Contact each vault and store that value in the vault.
func (s *ControlServer) post(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// We did not get a valid body from the client. Tell them so.
		glog.Warningf("Could not read body: %v\n", err)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Invalid or missing POST body"))
		return
	}
	n, e := strconv.Atoi(string(body))
	if n < 0 || e != nil {
		// We got a body, but it is not a valid integer (or not valid for us).
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Invalid or missing POST body"))
		return
	}
	// Check to make sure that this value is larger than the one we've previously committed
	s.lock.RLock()
	if n < s.minValue {
		msg := fmt.Sprintf("Client would make value decrease from %d to %d", s.minValue, n)
		s.lock.RUnlock()
		glog.Warning(msg)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(msg))
		return
	}
	s.lock.RUnlock()
	// Send the update to the vaults, keeping track of how many vaults actually responded to us.
	// Technically this is a set(), but because Go doesn't have sets, this is a map of vaults to
	// booleans, where the value stored in the map doesn't really matter. The presence of ANY
	// value is enough to show that we got a successful response from the vault.
	resp := make(map[string]bool)
	s.postValueToVaults(body, resp)
	// If the number of responses represents a majority of the vaults, then we can claim success
	// in storing this value in our system. Otherwise it represents a server failure.
	if s.hasMajority(len(resp)) {
		w.WriteHeader(http.StatusOK)
		// Set the min value here to prevent us from going backwards.
		s.lock.Lock()
		s.minValue = n
		s.lock.Unlock()
	} else {
		w.WriteHeader(http.StatusInternalServerError)
	}
	// In addition to the status code, unconditionally return a message of how many vaults we updated.
	w.Write([]byte(fmt.Sprintf("Sent updates to %d/%d vaults", len(resp), len(s.Vaults))))
}

// Actually send the POST commands to the vaults.
func (s *ControlServer) postValueToVaults(body []byte, resp map[string]bool) {
	// Use a WaitGroup so we can run the requests in parallel goroutine threads.
	var wg sync.WaitGroup
	// We will need to synchronize access to the response map.
	m := sync.RWMutex{}
	// For each vault, send a POST message containing the same body we received from the client.
	for _, vault := range s.Vaults {
		wg.Add(1)
		go func(m *sync.RWMutex, vault string, body []byte, resp map[string]bool) {
			defer wg.Done()
			glog.V(1).Infof("Setting vault %s value to %s", vault, string(body))
			url := fmt.Sprintf("http://%s/", vault)
			r, err := http.Post(url, "text/plain", bytes.NewBuffer(body))
			if err == nil && r.StatusCode == http.StatusOK {
				// Indicate that we received an OK from the vault.
				m.Lock()
				resp[url] = true
				m.Unlock()
			} else {
				// This could include a failure to connect or a timeout during the update.
				glog.Warningf("Error setting vault %s value to %s: %v", vault, string(body), err)
			}
		}(&m, vault, body, resp)
	}
	// Wait for all the connections to complete/timeout/fail.
	wg.Wait()
}

// Check if this number represents a majority of the vaults, where majority has to be >50%.
func (s *ControlServer) hasMajority(count int) bool {
	numVaults := len(s.Vaults)
	// By default this division will do the equivalent of math.Floor()
	numForMajority := (numVaults / 2) + 1
	return count >= numForMajority
}

func main() {
	portPtr := flag.Int("port", 8000, "Port on which to listen for requests")
	vaultsPtr := flag.String("vaults", "", "Comma-separated list of vaults")
	flag.Parse()
	s := NewControlServer(*vaultsPtr)
	err := http.ListenAndServe(fmt.Sprintf(":%d", *portPtr), s.mux)
	if errors.Is(err, http.ErrServerClosed) {
		fmt.Printf("server closed\n")
	} else if err != nil {
		fmt.Printf("error starting server: %s\n", err)
		os.Exit(1)
	}
}