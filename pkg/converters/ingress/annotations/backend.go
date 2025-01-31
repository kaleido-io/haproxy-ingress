/*
Copyright 2019 The HAProxy Ingress Controller Authors.

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

package annotations

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	ingutils "github.com/jcmoraisjr/haproxy-ingress/pkg/converters/ingress/utils"
	hatypes "github.com/jcmoraisjr/haproxy-ingress/pkg/haproxy/types"
	"github.com/jcmoraisjr/haproxy-ingress/pkg/utils"
)

func (c *updater) buildBackendAffinity(d *backData) {
	if d.ann.Affinity != "cookie" {
		if d.ann.Affinity != "" {
			c.logger.Error("unsupported affinity type on %v: %s", d.ann.Source, d.ann.Affinity)
		}
		return
	}
	name := d.ann.SessionCookieName
	if name == "" {
		name = "INGRESSCOOKIE"
	}
	strategy := d.ann.SessionCookieStrategy
	switch strategy {
	case "insert", "rewrite", "prefix":
	default:
		if strategy != "" {
			c.logger.Warn("invalid affinity cookie strategy '%s' on %v, using 'insert' instead", strategy, d.ann.Source)
		}
		strategy = "insert"
	}
	d.backend.Cookie.Name = name
	d.backend.Cookie.Strategy = strategy
	d.backend.Cookie.Dynamic = d.ann.SessionCookieDynamic
}

func (c *updater) buildBackendAuthHTTP(d *backData) {
	if d.ann.AuthType != "basic" {
		if d.ann.AuthType != "" {
			c.logger.Error("unsupported authentication type on %v: %s", d.ann.Source, d.ann.AuthType)
		}
		return
	}
	if d.ann.AuthSecret == "" {
		c.logger.Error("missing secret name on basic authentication on %v", d.ann.Source)
		return
	}
	secretName := ingutils.FullQualifiedName(d.ann.Source.Namespace, d.ann.AuthSecret)
	listName := strings.Replace(secretName, "/", "_", 1)
	userlist := c.haproxy.FindUserlist(listName)
	if userlist == nil {
		userb, err := c.cache.GetSecretContent(secretName, "auth")
		if err != nil {
			c.logger.Error("error reading basic authentication on %v: %v", d.ann.Source, err)
			return
		}
		userstr := string(userb)
		users, errs := c.buildBackendAuthHTTPExtractUserlist(d.ann.Source.Name, secretName, userstr)
		for _, err := range errs {
			c.logger.Warn("ignoring malformed usr/passwd on secret '%s', declared on %v: %v", secretName, d.ann.Source, err)
		}
		userlist = c.haproxy.AddUserlist(listName, users)
		if len(users) == 0 {
			c.logger.Warn("userlist on %v for basic authentication is empty", d.ann.Source)
		}
	}
	d.backend.Userlist.Name = userlist.Name
	realm := "localhost" // HAProxy's backend name would be used if missing
	if strings.Index(d.ann.AuthRealm, `"`) >= 0 {
		c.logger.Warn("ignoring auth-realm with quotes on %v", d.ann.Source)
	} else if d.ann.AuthRealm != "" {
		realm = d.ann.AuthRealm
	}
	d.backend.Userlist.Realm = realm
}

func (c *updater) buildBackendAuthHTTPExtractUserlist(source, secret, users string) ([]hatypes.User, []error) {
	var userlist []hatypes.User
	var err []error
	for i, usr := range strings.Split(users, "\n") {
		if usr == "" {
			continue
		}
		sep := strings.Index(usr, ":")
		if sep == -1 {
			err = append(err, fmt.Errorf("missing password of user '%s' line %d", usr, i+1))
			continue
		}
		username := usr[:sep]
		if username == "" {
			err = append(err, fmt.Errorf("missing username line %d", i+1))
			continue
		}
		if sep == len(usr)-1 || usr[sep:] == "::" {
			err = append(err, fmt.Errorf("missing password of user '%s' line %d", username, i+1))
			continue
		}
		var user hatypes.User
		if string(usr[sep+1]) == ":" {
			// usr::pwd
			user = hatypes.User{
				Name:      username,
				Passwd:    usr[sep+2:],
				Encrypted: false,
			}
		} else {
			// usr:pwd
			user = hatypes.User{
				Name:      username,
				Passwd:    usr[sep+1:],
				Encrypted: true,
			}
		}
		userlist = append(userlist, user)
	}
	return userlist, err
}

func (c *updater) buildBackendBlueGreen(d *backData) {
	balance := d.ann.BlueGreenBalance
	if balance == "" {
		balance = d.ann.BlueGreenDeploy
		if balance == "" {
			return
		}
	}
	type deployWeight struct {
		labelName  string
		labelValue string
		weight     int
		endpoints  []*hatypes.Endpoint
	}
	var deployWeights []*deployWeight
	for _, weight := range strings.Split(balance, ",") {
		dwSlice := strings.Split(weight, "=")
		if len(dwSlice) != 3 {
			c.logger.Error("blue/green config on %v has an invalid weight format: %s", d.ann.Source, weight)
			return
		}
		w, err := strconv.ParseInt(dwSlice[2], 10, 0)
		if err != nil {
			c.logger.Error("blue/green config on %v has an invalid weight value: %v", d.ann.Source, err)
			return
		}
		if w < 0 {
			c.logger.Warn("invalid weight '%d' on %v, using '0' instead", w, d.ann.Source)
			w = 0
		}
		if w > 256 {
			c.logger.Warn("invalid weight '%d' on %v, using '256' instead", w, d.ann.Source)
			w = 256
		}
		dw := &deployWeight{
			labelName:  dwSlice[0],
			labelValue: dwSlice[1],
			weight:     int(w),
		}
		deployWeights = append(deployWeights, dw)
	}
	for _, ep := range d.backend.Endpoints {
		if ep.Weight == 0 {
			// Draining endpoint, remove from blue/green calc
			continue
		}
		hasLabel := false
		if pod, err := c.cache.GetPod(ep.TargetRef); err == nil {
			for _, dw := range deployWeights {
				if label, found := pod.Labels[dw.labelName]; found {
					if label == dw.labelValue {
						// mode == pod and gcdGroupWeight == 0 need ep.Weight assgined,
						// otherwise ep.Weight will be rewritten after rebalance
						ep.Weight = dw.weight
						dw.endpoints = append(dw.endpoints, ep)
						hasLabel = true
					}
				}
			}
		} else {
			if ep.TargetRef == "" {
				err = fmt.Errorf("endpoint does not reference a pod")
			}
			c.logger.Warn("endpoint '%s:%d' on %v was removed from balance: %v", ep.IP, ep.Port, d.ann.Source, err)
		}
		if !hasLabel {
			// no label match, set weight as zero to remove new traffic
			// without remove from the balancer
			ep.Weight = 0
		}
	}
	for _, dw := range deployWeights {
		if len(dw.endpoints) == 0 {
			c.logger.InfoV(3, "blue/green balance label '%s=%s' on %v does not reference any endpoint", dw.labelName, dw.labelValue, d.ann.Source)
		}
	}
	if mode := d.ann.BlueGreenMode; mode == "pod" {
		// mode == pod, same weight as defined on balance annotation,
		// no need to rebalance
		return
	} else if mode != "" && mode != "deploy" {
		c.logger.Warn("unsupported blue/green mode '%s' on %v, falling back to 'deploy'", d.ann.BlueGreenMode, d.ann.Source)
	}
	// mode == deploy, need to recalc based on the number of replicas
	lcmCount := 0
	for _, dw := range deployWeights {
		count := len(dw.endpoints)
		if count == 0 {
			continue
		}
		if lcmCount > 0 {
			lcmCount = ingutils.LCM(lcmCount, count)
		} else {
			lcmCount = count
		}
	}
	if lcmCount == 0 {
		// all counts are zero, this config won't be used
		return
	}
	gcdGroupWeight := 0
	maxWeight := 0
	for _, dw := range deployWeights {
		count := len(dw.endpoints)
		if count == 0 || dw.weight == 0 {
			continue
		}
		groupWeight := dw.weight * lcmCount / count
		if gcdGroupWeight > 0 {
			gcdGroupWeight = ingutils.GCD(gcdGroupWeight, groupWeight)
		} else {
			gcdGroupWeight = groupWeight
		}
		if groupWeight > maxWeight {
			maxWeight = groupWeight
		}
	}
	if gcdGroupWeight == 0 {
		// all weights are zero, no need to rebalance
		return
	}
	// HAProxy weight must be between 0..256.
	// weightFactor has how many times the max weight is greater than 256.
	weightFactor := float32(maxWeight) / float32(gcdGroupWeight) / float32(256)
	// LCM of denominators and GCD of the results are known. Updating ep.Weight
	for _, dw := range deployWeights {
		for _, ep := range dw.endpoints {
			weight := dw.weight * lcmCount / len(dw.endpoints) / gcdGroupWeight
			if weightFactor > 1 {
				propWeight := int(float32(weight) / weightFactor)
				if propWeight == 0 && dw.weight > 0 {
					propWeight = 1
				}
				ep.Weight = propWeight
			} else {
				ep.Weight = weight
			}
		}
	}
}

var (
	corsOriginRegex  = regexp.MustCompile(`^(https?://[A-Za-z0-9\-\.]*(:[0-9]+)?|\*)?$`)
	corsMethodsRegex = regexp.MustCompile(`^([A-Za-z]+,?\s?)+$`)
	corsHeadersRegex = regexp.MustCompile(`^([A-Za-z0-9\-\_]+,?\s?)+$`)
)

func (c *updater) buildBackendCors(d *backData) {
	if !d.ann.CorsEnable {
		return
	}
	d.backend.Cors.Enabled = true
	if d.ann.CorsAllowOrigin != "" && corsOriginRegex.MatchString(d.ann.CorsAllowOrigin) {
		d.backend.Cors.AllowOrigin = d.ann.CorsAllowOrigin
	} else {
		d.backend.Cors.AllowOrigin = "*"
	}
	if corsHeadersRegex.MatchString(d.ann.CorsAllowHeaders) {
		d.backend.Cors.AllowHeaders = d.ann.CorsAllowHeaders
	} else {
		d.backend.Cors.AllowHeaders =
			"DNT,X-CustomHeader,Keep-Alive,User-Agent,X-Requested-With,If-Modified-Since,Cache-Control,Content-Type,Authorization"
	}
	if corsMethodsRegex.MatchString(d.ann.CorsAllowMethods) {
		d.backend.Cors.AllowMethods = d.ann.CorsAllowMethods
	} else {
		d.backend.Cors.AllowMethods = "GET, PUT, POST, DELETE, PATCH, OPTIONS"
	}
	d.backend.Cors.AllowCredentials = d.ann.CorsAllowCredentials
	if d.ann.CorsMaxAge > 0 {
		d.backend.Cors.MaxAge = d.ann.CorsMaxAge
	} else {
		d.backend.Cors.MaxAge = 86400
	}
	if corsHeadersRegex.MatchString(d.ann.CorsExposeHeaders) {
		d.backend.Cors.ExposeHeaders = d.ann.CorsExposeHeaders
	}
}

var (
	oauthHeaderRegex = regexp.MustCompile(`^[A-Za-z0-9-]+:[A-Za-z0-9-_]+$`)
)

func (c *updater) buildOAuth(d *backData) {
	if d.ann.OAuth == "" {
		return
	}
	if d.ann.OAuth != "oauth2_proxy" {
		c.logger.Warn("ignoring invalid oauth implementation '%s' on %v", d.ann.OAuth, d.ann.Source)
		return
	}
	uriPrefix := "/oauth2"
	headers := []string{"X-Auth-Request-Email:auth_response_email"}
	if d.ann.OAuthURIPrefix != "" {
		uriPrefix = d.ann.OAuthURIPrefix
	}
	if d.ann.OAuthHeaders != "" {
		headers = strings.Split(d.ann.OAuthHeaders, ",")
	}
	uriPrefix = strings.TrimRight(uriPrefix, "/")
	namespace := d.ann.Source.Namespace
	backend := c.findBackend(namespace, uriPrefix)
	if backend == nil {
		c.logger.Error("path '%s' was not found on namespace '%s'", uriPrefix, namespace)
		return
	}
	headersMap := make(map[string]string, len(headers))
	for _, header := range headers {
		if len(header) == 0 {
			continue
		}
		if !oauthHeaderRegex.MatchString(header) {
			c.logger.Warn("invalid header format '%s' on %v", header, d.ann.Source)
			continue
		}
		h := strings.Split(header, ":")
		headersMap[h[0]] = h[1]
	}
	d.backend.OAuth.Impl = d.ann.OAuth
	d.backend.OAuth.BackendName = backend.ID
	d.backend.OAuth.URIPrefix = uriPrefix
	d.backend.OAuth.Headers = headersMap
}

func (c *updater) findBackend(namespace, uriPrefix string) *hatypes.Backend {
	for _, host := range c.haproxy.Hosts() {
		for _, path := range host.Paths {
			if strings.TrimRight(path.Path, "/") == uriPrefix && path.Backend.Namespace == namespace {
				return path.Backend
			}
		}
	}
	return nil
}

var (
	rewriteURLRegex = regexp.MustCompile(`^[^"' ]+$`)
)

func (c *updater) buildRewriteURL(d *backData) {
	if d.ann.RewriteTarget == "" {
		return
	}
	if !rewriteURLRegex.MatchString(d.ann.RewriteTarget) {
		c.logger.Warn("rewrite-target does not allow white spaces or single/double quotes on %v", d.ann.Source)
		return
	}
	d.backend.RewriteURL = d.ann.RewriteTarget
}

func (c *updater) buildWAF(d *backData) {
	if d.ann.WAF == "" {
		return
	}
	if d.ann.WAF != "modsecurity" {
		c.logger.Warn("ignoring invalid WAF mode: %s", d.ann.WAF)
		return
	}
	d.backend.WAF = d.ann.WAF
}

func (c *updater) buildWhitelist(d *backData) {
	if d.ann.WhitelistSourceRange == "" {
		return
	}
	var cidrlist []string
	for _, cidr := range utils.Split(d.ann.WhitelistSourceRange, ",") {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			c.logger.Warn("skipping invalid cidr '%s' in whitelist config on %v", cidr, d.ann.Source)
		} else {
			cidrlist = append(cidrlist, cidr)
		}
	}
	d.backend.Whitelist = cidrlist
}
