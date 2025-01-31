// Package winbind is the winbind backend for fetching AD active configuration and online status.
package winbind

/*
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <stdbool.h>
#include <stdio.h>
#include <errno.h>

#include <wbclient.h>

char *get_domain_name() {
  // Get domain name
  wbcErr wbc_status = WBC_ERR_UNKNOWN_FAILURE;
  struct wbcInterfaceDetails *info;

  wbc_status = wbcInterfaceDetails(&info);
  if (wbc_status != WBC_ERR_SUCCESS || info->dns_domain == NULL) {
    return NULL;
  }
  return strdup(info->dns_domain);
}

char *get_dc_name(char *domain) {
  // Get DC name from domain name
  wbcErr wbc_status = WBC_ERR_UNKNOWN_FAILURE;
  struct wbcDomainControllerInfo *dc_info = NULL;

  wbc_status = wbcLookupDomainController(domain, WBC_LOOKUP_DC_DS_REQUIRED, &dc_info);
  if (wbc_status != WBC_ERR_SUCCESS || dc_info->dc_name == NULL) {
    return NULL;
  }
  return strdup(dc_info->dc_name);
}

bool is_online(char *domain) {
  wbcErr wbc_status = WBC_ERR_UNKNOWN_FAILURE;
  struct wbcDomainInfo *info = NULL;

  wbc_status = wbcDomainInfo(domain, &info);
  if (wbc_status != WBC_ERR_SUCCESS) {
    // Since there's no general purpose errno that we can use, set it to
    // whatever wbc_status is and have the caller print the status code.
    errno = wbc_status;
    return false;
  }
  return !(info->domain_flags & WBC_DOMINFO_DOMAIN_OFFLINE);
}
*/
// #cgo pkg-config: wbclient
import "C"

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unsafe"

	"github.com/leonelquinteros/gotext"
	log "github.com/ubuntu/adsys/internal/grpc/logstreamer"
	"github.com/ubuntu/adsys/internal/smbsafe"
	"github.com/ubuntu/decorate"
)

// Winbind is the backend object with domain and DC information.
type Winbind struct {
	staticServerFQDN    string
	domain              string
	defaultDomainSuffix string
	kinitCmd            []string
	hostname            string

	config Config
}

// Config for winbind backend.
type Config struct {
	ADServer string `mapstructure:"ad_server"` // bypass winbind and use this server
	ADDomain string `mapstructure:"ad_domain"` // bypass domain name detection and use this domain
}

// Option represents an optional function to change the winbind backend.
type Option func(*options)

type options struct {
	kinitCmd []string
}

// New returns a winbind backend loaded from Config.
func New(ctx context.Context, c Config, hostname string, opts ...Option) (w Winbind, err error) {
	defer decorate.OnError(&err, gotext.Get("can't get domain configuration from %+v", c))

	// defaults
	args := options{
		kinitCmd: []string{"kinit"},
	}
	// applied options
	for _, o := range opts {
		o(&args)
	}

	log.Debug(ctx, "Loading Winbind configuration for AD backend")

	if c.ADDomain == "" {
		c.ADDomain, err = domainName()
		if err != nil {
			return Winbind{}, err
		}
	}

	return Winbind{
		staticServerFQDN:    c.ADServer,
		domain:              c.ADDomain,
		defaultDomainSuffix: c.ADDomain,
		kinitCmd:            args.kinitCmd,
		hostname:            hostname,
		config:              c,
	}, nil
}

// Domain returns current server domain.
func (w Winbind) Domain() string {
	return w.domain
}

// HostKrb5CCName returns the absolute path of the machine krb5 ticket.
func (w Winbind) HostKrb5CCName() (string, error) {
	target := "/tmp/krb5cc_0"

	if os.Getenv("ADSYS_SKIP_ROOT_CALLS") != "" {
		return target, nil
	}
	// Uppercase domain and hostname
	domain := strings.ToUpper(w.domain)
	hostname := strings.ToUpper(w.hostname)

	principal := fmt.Sprintf("%s$@%s", hostname, domain)
	cmdArgs := append(w.kinitCmd, "-k", principal, "-c", target)
	smbsafe.WaitExec()
	defer smbsafe.DoneExec()
	if cmd, err := exec.Command(cmdArgs[0], cmdArgs[1:]...).CombinedOutput(); err != nil {
		return "", errors.New(gotext.Get(`could not get krb5 cached ticket for %q: %v:
%s`, principal, err, string(cmd)))
	}

	return target, nil
}

// DefaultDomainSuffix returns current default domain suffix.
func (w Winbind) DefaultDomainSuffix() string {
	return w.defaultDomainSuffix
}

// ServerFQDN returns current server FQDN.
// It returns first any static configuration. If nothing is found, it will fetch
// the active server from winbind.
func (w Winbind) ServerFQDN(ctx context.Context) (serverFQDN string, err error) {
	defer decorate.OnError(&err, gotext.Get("error while trying to look up AD server address on winbind"))

	if w.staticServerFQDN != "" {
		return strings.TrimPrefix(w.staticServerFQDN, "ldap://"), nil
	}

	log.Debugf(ctx, "Triggering autodiscovery of AD server because winbind configuration does not provide an ad_server for %q", w.domain)
	serverFQDN, err = dcName(w.domain)
	if err != nil {
		return "", err
	}
	serverFQDN = strings.TrimPrefix(serverFQDN, `\\`)

	return serverFQDN, nil
}

// Config returns a stringified configuration for Winbind backend.
func (w Winbind) Config() string {
	return "Current backend is Winbind"
}

// IsOnline refresh and returns if we are online.
func (w Winbind) IsOnline() (bool, error) {
	cDomain := C.CString(w.domain)
	defer C.free(unsafe.Pointer(cDomain))
	online, err := C.is_online(cDomain)
	if err != nil {
		err = errors.New(gotext.Get("could not get online status for domain %q: status code %d", w.domain, err))
	}
	return bool(online), err
}

func domainName() (string, error) {
	dc := C.get_domain_name()
	if dc == nil {
		return "", errors.New(gotext.Get("could not get domain name"))
	}
	defer C.free(unsafe.Pointer(dc))
	return C.GoString(dc), nil
}

func dcName(domain string) (string, error) {
	cDomain := C.CString(domain)
	defer C.free(unsafe.Pointer(cDomain))
	dc := C.get_dc_name(cDomain)
	if dc == nil {
		return "", errors.New(gotext.Get("could not get domain controller name for domain %q", domain))
	}
	defer C.free(unsafe.Pointer(dc))
	return C.GoString(dc), nil
}
