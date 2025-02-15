/*
Copyright 2017 The Nuclio Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/nuclio/nuclio/pkg/common"
	"github.com/nuclio/nuclio/pkg/common/headers"
	"github.com/nuclio/nuclio/pkg/functionconfig"
	nuctlcommon "github.com/nuclio/nuclio/pkg/nuctl/command/common"

	"github.com/nuclio/errors"
	"github.com/nuclio/logger"
	"k8s.io/apimachinery/pkg/labels"
)

const DefaultRequestTimeout = "60s"

type NuclioAPIClient struct {
	logger         logger.Logger
	httpClient     *http.Client
	apiURL         string
	requestTimeout string
	username       string
	password       string
	skipTLSVerify  bool
	authHeaders    map[string]string
}

func NewNuclioAPIClient(parentLogger logger.Logger,
	apiURL string,
	requestTimeout string,
	username string,
	password string,
	skipTLSVerify bool) (*NuclioAPIClient, error) {
	newAPIClient := &NuclioAPIClient{
		logger:         parentLogger.GetChild("api-client"),
		apiURL:         apiURL,
		requestTimeout: requestTimeout,
		username:       username,
		password:       password,
		skipTLSVerify:  skipTLSVerify,
	}

	// parse the request timeout
	if requestTimeout == "" {
		requestTimeout = DefaultRequestTimeout
	}
	requestTimeoutDuration, err := time.ParseDuration(requestTimeout)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to parse request timeout")
	}

	// create HTTP client
	newAPIClient.httpClient = &http.Client{
		Timeout: requestTimeoutDuration,
	}
	if skipTLSVerify {
		newAPIClient.httpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	return newAPIClient, nil
}

// GetFunctions returns a map of function name to function config for all functions in the given namespace
func (c *NuclioAPIClient) GetFunctions(ctx context.Context, namespace string) (map[string]functionconfig.Config, error) {

	url := fmt.Sprintf("%s/%s", c.apiURL, FunctionsEndpoint)
	requestHeaders := map[string]string{
		headers.FunctionNamespace: namespace,
	}
	_, responseBody, err := c.sendRequest(ctx,
		http.MethodGet, // method
		url,            // url
		nil,            // body
		requestHeaders, // headers
		http.StatusOK,  // expectedStatusCode
		true)           // returnResponseBody
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get functions")
	}

	c.logger.DebugWithCtx(ctx, "Got functions", "numOfFunctions", len(responseBody))

	functions := map[string]functionconfig.Config{}

	for functionName, functionConfigMap := range responseBody {
		functionConfig, err := nuctlcommon.ConvertMapToFunctionConfig(functionConfigMap.(map[string]interface{}))
		if err != nil {
			return nil, errors.Wrap(err, "Failed to convert function config")
		}

		functions[functionName] = functionConfig
	}

	return functions, nil
}

// PatchFunction patches a single function with the given options
func (c *NuclioAPIClient) PatchFunction(ctx context.Context,
	functionName,
	namespace string,
	optionsPayload []byte,
	patchHeaders map[string]string) error {

	c.logger.DebugWithCtx(ctx, "Patching function", "function", functionName)

	url := fmt.Sprintf("%s/%s/%s", c.apiURL, FunctionsEndpoint, functionName)

	if _, _, err := c.sendRequest(ctx,
		http.MethodPatch,
		url,
		optionsPayload,
		patchHeaders,
		http.StatusNoContent,
		false); err != nil {
		return errors.Wrap(err, "Failed to send patch API request")
	}

	return nil
}

// sendRequest sends an API request to the nuclio API
func (c *NuclioAPIClient) sendRequest(ctx context.Context,
	method,
	url string,
	requestBody []byte,
	requestHeaders map[string]string,
	expectedStatusCode int,
	returnResponseBody bool) (*http.Response, map[string]interface{}, error) {
	c.logger.DebugWithCtx(ctx,
		"Sending API request",
		"method", method,
		"url", url,
		"headers", requestHeaders,
		"body", string(requestBody))

	// create authorization headers
	authHeaders, err := c.createAuthorizationHeaders(ctx)
	if err != nil {
		return nil, nil, errors.Wrap(err, "Failed to create session")
	}

	req, err := http.NewRequest(method, url, bytes.NewBuffer(requestBody))
	if err != nil {
		return nil, nil, errors.Wrap(err, "Failed to create request")
	}

	req.Header.Set("Content-Type", "application/json")
	for key, value := range labels.Merge(requestHeaders, authHeaders) {
		req.Header.Set(key, value)
	}

	response, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, errors.Wrap(err, "Failed to send request")
	}

	if response.StatusCode != expectedStatusCode {
		return nil, nil, errors.Errorf("Expected status code %d, got %d", expectedStatusCode, response.StatusCode)
	}

	if !returnResponseBody {
		return response, nil, nil
	}

	encodedResponseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, nil, errors.Wrap(err, "Failed to read response body")
	}

	defer response.Body.Close() // nolint: errcheck

	decodedResponseBody := map[string]interface{}{}
	if err := json.Unmarshal(encodedResponseBody, &decodedResponseBody); err != nil {
		return nil, nil, errors.Wrap(err, "Failed to decode response body")
	}

	return response, decodedResponseBody, nil
}

// createAuthorizationHeaders creates authorization headers for the nuclio API
func (c *NuclioAPIClient) createAuthorizationHeaders(ctx context.Context) (map[string]string, error) {
	if c.authHeaders != nil {
		return c.authHeaders, nil
	}

	// resolve username and password from env vars if not provided
	if c.username == "" {
		c.username = common.GetEnvOrDefaultString("NUCLIO_USERNAME", "")
	}
	if c.password == "" {
		c.password = common.GetEnvOrDefaultString("NUCLIO_PASSWORD", "")
	}

	// if username and password are still empty, fail
	if c.username == "" || c.password == "" {
		return nil, errors.New("Username and password must be provided")
	}

	// cache the auth headers
	c.authHeaders = map[string]string{
		"x-v3io-username": c.username,
		"Authorization":   "Basic " + base64.StdEncoding.EncodeToString([]byte(c.username+":"+c.password)),
	}

	return c.authHeaders, nil
}
