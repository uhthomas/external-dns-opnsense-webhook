package opnsense

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	log "github.com/sirupsen/logrus"
	"sigs.k8s.io/external-dns/endpoint"
)

// httpClient is the DNS provider client.
type httpClient struct {
	*Config
	*http.Client
}

const (
	opnsenseUnboundServicePath  = "%s/api/unbound/service/%s"
	opnsenseUnboundSettingsPath = "%s/api/unbound/settings/%s"
	// Hacky, but nice to have the delete as an explicit constant since it's destructive
	opnsenseUnboundSettingsPathDelete = "%s/api/unbound/settings/delHostOverride/%s"
)

// newOpnsenseClient creates a new DNS provider client.
func newOpnsenseClient(config *Config) (*httpClient, error) {

	// Create the HTTP client
	client := &httpClient{
		Config: config,
		Client: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: config.SkipTLSVerify},
			},
		},
	}

	if err := client.login(); err != nil {
		return nil, err
	}

	return client, nil
}

// login performs a basic call to validate credentials
func (c *httpClient) login() error {

	// Perform the test call by getting service status
	resp, err := c.doRequest(
		http.MethodGet,
		FormatUrl(opnsenseUnboundServicePath, c.Config.Host, "status"),
		nil,
	)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	// Check if the login was successful
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		log.Errorf("login: failed: %s, response: %s", resp.Status, string(respBody))
		return fmt.Errorf("login: failed: %s", resp.Status)
	}

	return nil
}

// doRequest makes an HTTP request to the Opnsense firewall.
func (c *httpClient) doRequest(method, path string, body io.Reader) (*http.Response, error) {
	log.Debugf("doRequest: making %s request to %s", method, path)

	req, err := http.NewRequest(method, path, body)
	if err != nil {
		return nil, err
	}

	c.setHeaders(req)

	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}

	log.Debugf("doRequest: response code from %s request to %s: %d", method, path, resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("doRequest: %s request to %s was not successful: %d", method, path, resp.StatusCode)
	}

	return resp, nil
}

// GetHostOverrides retrieves the list of HostOverrides from the Opnsense Firewall's Unbound API.
// These are equivalent to A or AAAA records
func (c *httpClient) GetHostOverrides() ([]DNSRecord, error) {

	resp, err := c.doRequest(
		http.MethodGet,
		FormatUrl(opnsenseUnboundSettingsPath, c.Config.Host, "searchHostOverride"),
		nil,
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var records unboundRecordsList
	if err = json.NewDecoder(resp.Body).Decode(&records); err != nil {
		return nil, err
	}

	log.Debugf("gethost: retrieved records: %+v", records.Rows)

	return records.Rows, nil
}

// CreateHostOverride creates a new DNS A or AAAA record in the Opnsense Firewall's Unbound API.
func (c *httpClient) CreateHostOverride(endpoint *endpoint.Endpoint) (*DNSRecord, error) {

	log.Debugf("create: Try pulling pre-existing Unbound %s record: %s", endpoint.RecordType, endpoint.DNSName)
	lookup, err := c.lookupHostOverrideIdentifier(endpoint.DNSName, endpoint.RecordType)
	if err != nil {
		return nil, err
	}

	if lookup != nil {
		log.Debugf("create: Found uuid: %s", lookup.Uuid)
		log.Debugf("create: Found existing %s record for %s : %s", endpoint.RecordType, endpoint.DNSName, lookup.Uuid)
		return lookup, nil
	}

	SplittedHost := UnboundFQDNSplitter(endpoint.DNSName)

	jsonBody, err := json.Marshal(unboundAddHostOverride{
		Host: DNSRecord{
			Enabled:  "1",
			Rr:       endpoint.RecordType,
			Server:   endpoint.Targets[0],
			Hostname: SplittedHost[0],
			Domain:   SplittedHost[1],
		}})
	if err != nil {
		return nil, err
	}

	log.Debugf("create: POST: %s", string(jsonBody))
	resp, err := c.doRequest(
		http.MethodPost,
		FormatUrl(opnsenseUnboundSettingsPath, c.Config.Host, "addHostOverride"),
		bytes.NewReader(jsonBody),
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// TODO: Better error handling if API returns:
	// {"result":"failed"}
	//if resp.Body != nil && resp.Body

	var record unboundAddHostOverride
	if err = json.NewDecoder(resp.Body).Decode(&record); err != nil {
		return nil, err
	}
	log.Debugf("create: created record: %+v", record)

	return nil, nil
}

// DeleteHostOverride deletes a DNS record from the Opnsense Firewall's Unbound API.
func (c *httpClient) DeleteHostOverride(endpoint *endpoint.Endpoint) error {
	log.Debugf("delete: Deleting record %+v", endpoint)
	lookup, err := c.lookupHostOverrideIdentifier(endpoint.DNSName, endpoint.RecordType)
	if err != nil {
		return err
	}

	log.Debugf("delete: Found match %s", lookup.Uuid)

	// empty json is required for this POST to work
	var q struct{}

	jsonBody, err := json.Marshal(q)
	if err != nil {
		return err
	}

	log.Debugf("delete: Sending POST %s", lookup.Uuid)
	if _, err = c.doRequest(
		http.MethodPost,
		FormatUrl(opnsenseUnboundSettingsPathDelete, c.Config.Host, lookup.Uuid),
		bytes.NewReader(jsonBody),
	); err != nil {
		return err
	}

	return nil
}

// lookupHostOverrideIdentifier finds a HostOverride in the Opnsense Firewall's Unbound API.
func (c *httpClient) lookupHostOverrideIdentifier(key, recordType string) (*DNSRecord, error) {
	records, err := c.GetHostOverrides()
	if err != nil {
		return nil, err
	}
	log.Debug("lookup: Splitting FQDN")
	SplittedHost := UnboundFQDNSplitter(key)

	for _, r := range records {
		log.Debugf("lookup: Checking record: Host=%s, Domain=%s, Type=%s, UUID=%s", r.Hostname, r.Domain, UnboundTypeEmbellisher(r.Rr), r.Uuid)
		if r.Hostname == SplittedHost[0] && r.Domain == SplittedHost[1] && UnboundTypeEmbellisher(r.Rr) == UnboundTypeEmbellisher(recordType) {
			log.Debugf("lookup: UUID Match Found: %s", r.Uuid)
			return &r, nil
		}
	}
	log.Debugf("lookup: No matching record found for Host=%s, Domain=%s, Type=%s", SplittedHost[0], SplittedHost[1], UnboundTypeEmbellisher(recordType))
	return nil, nil
}

// ReconfigureUnbound performs a reconfigure action in Unbound after editing records
func (c *httpClient) ReconfigureUnbound() error {

	// empty json is required for this POST to work
	var q struct{}

	jsonBody, err := json.Marshal(q)
	if err != nil {
		return err
	}

	// Perform the reconfigure
	resp, err := c.doRequest(
		http.MethodPost,
		FormatUrl(opnsenseUnboundServicePath, c.Config.Host, "reconfigure"),
		bytes.NewReader(jsonBody),
	)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	// Check if the login was successful
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		log.Errorf("reconfigure: login failed: %s, response: %s", resp.Status, string(respBody))
		return fmt.Errorf("reconfigure: unbound failed: %s", resp.Status)
	}

	return nil
}

// setHeaders sets the headers for the HTTP request.
func (c *httpClient) setHeaders(req *http.Request) {
	// Add basic auth header
	opnsenseAuth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", c.Config.Key, c.Config.Secret)))
	req.Header.Add("Authorization", fmt.Sprintf("Basic %s", opnsenseAuth))
	req.Header.Add("Accept", "application/json")
	if req.Method != http.MethodGet {
		req.Header.Add("Content-Type", "application/json; charset=utf-8")
	}
	// Log the request URL
	log.Debugf("headers: Requesting %s", req.URL)
}
