package acme

import (
	"fmt"
	"strconv"
	"time"

	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/challenge/http01"
	"github.com/go-acme/lego/v4/challenge/tlsalpn01"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/http/memcached"
	"github.com/go-acme/lego/v4/providers/http/webroot"
	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/vancluever/terraform-provider-acme/v2/acme/dnsplugin"
)

// setCertificateChallengeProviders sets all of the challenge providers in the
// client that are needed for obtaining the certificate.
//
// The returned func() is a closer for all of the configured DNS providers that
// should be called when they are no longer needed (i.e. in a defer after one of
// the CRUD functions are complete).
func setCertificateChallengeProviders(client *lego.Client, d *schema.ResourceData) (func(), error) {
	// DNS
	dnsClosers := make([]func(), 0)
	dnsCloser := func() {
		for _, f := range dnsClosers {
			f()
		}
	}

	if providers, ok := d.GetOk("dns_challenge"); ok {
		dnsProvider, err := NewDNSProviderWrapper()
		if err != nil {
			return dnsCloser, err
		}

		for _, providerRaw := range providers.([]interface{}) {
			if p, closer, err := expandDNSChallenge(providerRaw.(map[string]interface{})); err == nil {
				dnsProvider.providers = append(dnsProvider.providers, p)
				dnsClosers = append(dnsClosers, closer)
			} else {
				return dnsCloser, err
			}
		}

		if err := client.Challenge.SetDNS01Provider(dnsProvider, expandDNSChallengeOptions(d)...); err != nil {
			return dnsCloser, err
		}
	}

	// HTTP (server)
	if provider, ok := d.GetOk("http_challenge"); ok {
		opts := provider.([]interface{})[0].(map[string]interface{})
		httpServerProvider := http01.NewProviderServer("", strconv.Itoa(opts["port"].(int)))
		if proxyHeader, ok := opts["proxy_header"]; ok {
			httpServerProvider.SetProxyHeader(proxyHeader.(string))
		}

		if err := client.Challenge.SetHTTP01Provider(httpServerProvider); err != nil {
			return dnsCloser, err
		}
	}

	// HTTP (webroot)
	if provider, ok := d.GetOk("http_webroot_challenge"); ok {
		httpWebrootProvider, err := webroot.NewHTTPProvider(
			provider.([]interface{})[0].(map[string]interface{})["directory"].(string))

		if err != nil {
			return dnsCloser, err
		}

		if err := client.Challenge.SetHTTP01Provider(httpWebrootProvider); err != nil {
			return dnsCloser, err
		}
	}

	// HTTP (memcached)
	if provider, ok := d.GetOk("http_memcached_challenge"); ok {
		httpMemcachedProvider, err := memcached.NewMemcachedProvider(
			stringSlice(provider.([]interface{})[0].(map[string]interface{})["hosts"].(*schema.Set).List()))

		if err != nil {
			return dnsCloser, err
		}

		if err := client.Challenge.SetHTTP01Provider(httpMemcachedProvider); err != nil {
			return dnsCloser, err
		}
	}

	// TLS
	if provider, ok := d.GetOk("tls_challenge"); ok {
		tlsProvider := tlsalpn01.NewProviderServer(
			"", strconv.Itoa(provider.([]interface{})[0].(map[string]interface{})["port"].(int)))

		if err := client.Challenge.SetTLSALPN01Provider(tlsProvider); err != nil {
			return dnsCloser, err
		}
	}

	return dnsCloser, nil
}

func expandDNSChallenge(m map[string]interface{}) (challenge.ProviderTimeout, func(), error) {
	var providerName string

	if v, ok := m["provider"]; ok && v.(string) != "" {
		providerName = v.(string)
	} else {
		return nil, nil, fmt.Errorf("DNS challenge provider not defined")
	}

	// Config only needs to be set if it's defined, otherwise existing env/SDK
	// defaults are fine.
	config := make(map[string]string)
	if v, ok := m["config"]; ok {
		for k, v := range v.(map[string]interface{}) {
			config[k] = v.(string)
		}
	}

	return dnsplugin.NewClient(providerName, config)
}

func expandDNSChallengeOptions(d *schema.ResourceData) []dns01.ChallengeOption {
	var opts []dns01.ChallengeOption
	if nameservers := d.Get("recursive_nameservers").([]interface{}); len(nameservers) > 0 {
		var s []string
		for _, ns := range nameservers {
			s = append(s, ns.(string))
		}

		opts = append(opts, dns01.AddRecursiveNameservers(s))
	}

	if d.Get("disable_complete_propagation").(bool) {
		opts = append(opts, dns01.DisableCompletePropagationRequirement())
	}

	if preCheckDelay := d.Get("pre_check_delay").(int); preCheckDelay > 0 {
		opts = append(opts, dns01.WrapPreCheck(resourceACMECertificatePreCheckDelay(preCheckDelay)))
	}

	return opts
}

// DNSProviderWrapper is a multi-provider wrapper to support multiple
// DNS challenges.
type DNSProviderWrapper struct {
	providers []challenge.ProviderTimeout
}

// NewDNSProviderWrapper returns an freshly initialized
// DNSProviderWrapper.
func NewDNSProviderWrapper() (*DNSProviderWrapper, error) {
	return &DNSProviderWrapper{}, nil
}

// Present implements challenge.Provider for DNSProviderWrapper.
func (d *DNSProviderWrapper) Present(domain, token, keyAuth string) error {
	var err error
	for _, p := range d.providers {
		err = p.Present(domain, token, keyAuth)
		if err != nil {
			err = multierror.Append(err, fmt.Errorf("error encountered while presenting token for DNS challenge: %s", err.Error()))
		}
	}

	return err
}

// CleanUp implements challenge.Provider for DNSProviderWrapper.
func (d *DNSProviderWrapper) CleanUp(domain, token, keyAuth string) error {
	var err error
	for _, p := range d.providers {
		err = p.CleanUp(domain, token, keyAuth)
		if err != nil {
			err = multierror.Append(err, fmt.Errorf("error encountered while cleaning token for DNS challenge: %s", err.Error()))
		}
	}

	return err
}

// Timeout implements challenge.ProviderTimeout for
// DNSProviderWrapper.
//
// The highest polling interval and timeout values defined across all
// providers is used.
func (d *DNSProviderWrapper) Timeout() (time.Duration, time.Duration) {
	var timeout, interval time.Duration
	for _, p := range d.providers {
		t, i := p.Timeout()
		if t > timeout {
			timeout = t
		}

		if i > interval {
			interval = i
		}
	}

	if timeout < 1 {
		timeout = dns01.DefaultPropagationTimeout
	}

	if interval < 1 {
		interval = dns01.DefaultPollingInterval
	}

	return timeout, interval
}
