// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"
)

// startTLS starts the TLS handshake.
func (c *Conn) startTLS(now time.Time, initialConnID []byte, params transportParameters) error {
	clientKeys, serverKeys := initialKeys(initialConnID)
	if c.side == clientSide {
		c.wkeys[initialSpace], c.rkeys[initialSpace] = clientKeys, serverKeys
	} else {
		c.wkeys[initialSpace], c.rkeys[initialSpace] = serverKeys, clientKeys
	}

	qconfig := &tls.QUICConfig{TLSConfig: c.config.TLSConfig}
	if c.side == clientSide {
		c.tls = tls.QUICClient(qconfig)
	} else {
		c.tls = tls.QUICServer(qconfig)
	}
	c.tls.SetTransportParameters(marshalTransportParameters(params))
	// TODO: We don't need or want a context for cancelation here,
	// but users can use a context to plumb values through to hooks defined
	// in the tls.Config. Pass through a context.
	if err := c.tls.Start(context.TODO()); err != nil {
		return err
	}
	return c.handleTLSEvents(now)
}

func (c *Conn) handleTLSEvents(now time.Time) error {
	for {
		e := c.tls.NextEvent()
		if c.testHooks != nil {
			c.testHooks.handleTLSEvent(e)
		}
		switch e.Kind {
		case tls.QUICNoEvent:
			return nil
		case tls.QUICSetReadSecret:
			space, k, err := tlsKey(e)
			if err != nil {
				return err
			}
			c.rkeys[space] = k
		case tls.QUICSetWriteSecret:
			space, k, err := tlsKey(e)
			if err != nil {
				return err
			}
			c.wkeys[space] = k
		case tls.QUICWriteData:
			space, err := spaceForLevel(e.Level)
			if err != nil {
				return err
			}
			c.crypto[space].write(e.Data)
		case tls.QUICHandshakeDone:
			if c.side == serverSide {
				// "[...] the TLS handshake is considered confirmed
				// at the server when the handshake completes."
				// https://www.rfc-editor.org/rfc/rfc9001#section-4.1.2-1
				c.confirmHandshake(now)
				if !c.config.TLSConfig.SessionTicketsDisabled {
					if err := c.tls.SendSessionTicket(false); err != nil {
						return err
					}
				}
			}
		case tls.QUICTransportParameters:
			params, err := unmarshalTransportParams(e.Data)
			if err != nil {
				return err
			}
			c.receiveTransportParameters(params)
		}
	}
}

// tlsKey returns the keys in a QUICSetReadSecret or QUICSetWriteSecret event.
func tlsKey(e tls.QUICEvent) (numberSpace, keys, error) {
	space, err := spaceForLevel(e.Level)
	if err != nil {
		return 0, keys{}, err
	}
	k, err := newKeys(e.Suite, e.Data)
	if err != nil {
		return 0, keys{}, err
	}
	return space, k, nil
}

func spaceForLevel(level tls.QUICEncryptionLevel) (numberSpace, error) {
	switch level {
	case tls.QUICEncryptionLevelInitial:
		return initialSpace, nil
	case tls.QUICEncryptionLevelHandshake:
		return handshakeSpace, nil
	case tls.QUICEncryptionLevelApplication:
		return appDataSpace, nil
	default:
		return 0, fmt.Errorf("quic: internal error: write handshake data at level %v", level)
	}
}

// handleCrypto processes data received in a CRYPTO frame.
func (c *Conn) handleCrypto(now time.Time, space numberSpace, off int64, data []byte) error {
	var level tls.QUICEncryptionLevel
	switch space {
	case initialSpace:
		level = tls.QUICEncryptionLevelInitial
	case handshakeSpace:
		level = tls.QUICEncryptionLevelHandshake
	case appDataSpace:
		level = tls.QUICEncryptionLevelApplication
	default:
		return errors.New("quic: internal error: received CRYPTO frame in unexpected number space")
	}
	err := c.crypto[space].handleCrypto(off, data, func(b []byte) error {
		return c.tls.HandleData(level, b)
	})
	if err != nil {
		return err
	}
	return c.handleTLSEvents(now)
}
