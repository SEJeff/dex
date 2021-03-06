package sql

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/coreos/dex/storage"
)

// TODO(ericchiang): The update, insert, and select methods queries are all
// very repetivite. Consider creating them programatically.

// keysRowID is the ID of the only row we expect to populate the "keys" table.
const keysRowID = "keys"

// encoder wraps the underlying value in a JSON marshaler which is automatically
// called by the database/sql package.
//
//		s := []string{"planes", "bears"}
//		err := db.Exec(`insert into t1 (id, things) values (1, $1)`, encoder(s))
//		if err != nil {
//			// handle error
//		}
//
//		var r []byte
//		err = db.QueryRow(`select things from t1 where id = 1;`).Scan(&r)
//		if err != nil {
//			// handle error
//		}
//		fmt.Printf("%s\n", r) // ["planes","bears"]
//
func encoder(i interface{}) driver.Valuer {
	return jsonEncoder{i}
}

// decoder wraps the underlying value in a JSON unmarshaler which can then be passed
// to a database Scan() method.
func decoder(i interface{}) sql.Scanner {
	return jsonDecoder{i}
}

type jsonEncoder struct {
	i interface{}
}

func (j jsonEncoder) Value() (driver.Value, error) {
	b, err := json.Marshal(j.i)
	if err != nil {
		return nil, fmt.Errorf("marshal: %v", err)
	}
	return b, nil
}

type jsonDecoder struct {
	i interface{}
}

func (j jsonDecoder) Scan(dest interface{}) error {
	if dest == nil {
		return errors.New("nil value")
	}
	b, ok := dest.([]byte)
	if !ok {
		return fmt.Errorf("expected []byte got %T", dest)
	}
	if err := json.Unmarshal(b, &j.i); err != nil {
		return fmt.Errorf("unmarshal: %v", err)
	}
	return nil
}

// Abstract conn vs trans.
type querier interface {
	QueryRow(query string, args ...interface{}) *sql.Row
}

// Abstract row vs rows.
type scanner interface {
	Scan(dest ...interface{}) error
}

func (c *conn) GarbageCollect(now time.Time) (result storage.GCResult, err error) {
	r, err := c.Exec(`delete from auth_request where expiry < $1`, now)
	if err != nil {
		return result, fmt.Errorf("gc auth_request: %v", err)
	}
	if n, err := r.RowsAffected(); err == nil {
		result.AuthRequests = n
	}

	r, err = c.Exec(`delete from auth_code where expiry < $1`, now)
	if err != nil {
		return result, fmt.Errorf("gc auth_code: %v", err)
	}
	if n, err := r.RowsAffected(); err == nil {
		result.AuthCodes = n
	}
	return
}

func (c *conn) CreateAuthRequest(a storage.AuthRequest) error {
	_, err := c.Exec(`
		insert into auth_request (
			id, client_id, response_types, scopes, redirect_uri, nonce, state,
			force_approval_prompt, logged_in,
			claims_user_id, claims_username, claims_email, claims_email_verified,
			claims_groups,
			connector_id, connector_data,
			expiry
		)
		values (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17
		);
	`,
		a.ID, a.ClientID, encoder(a.ResponseTypes), encoder(a.Scopes), a.RedirectURI, a.Nonce, a.State,
		a.ForceApprovalPrompt, a.LoggedIn,
		a.Claims.UserID, a.Claims.Username, a.Claims.Email, a.Claims.EmailVerified,
		encoder(a.Claims.Groups),
		a.ConnectorID, a.ConnectorData,
		a.Expiry,
	)
	if err != nil {
		return fmt.Errorf("insert auth request: %v", err)
	}
	return nil
}

func (c *conn) UpdateAuthRequest(id string, updater func(a storage.AuthRequest) (storage.AuthRequest, error)) error {
	return c.ExecTx(func(tx *trans) error {
		r, err := getAuthRequest(tx, id)
		if err != nil {
			return err
		}

		a, err := updater(r)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`
			update auth_request
			set
				client_id = $1, response_types = $2, scopes = $3, redirect_uri = $4,
				nonce = $5, state = $6, force_approval_prompt = $7, logged_in = $8,
				claims_user_id = $9, claims_username = $10, claims_email = $11,
				claims_email_verified = $12,
				claims_groups = $13,
				connector_id = $14, connector_data = $15,
				expiry = $16
			where id = $17;
		`,
			a.ClientID, encoder(a.ResponseTypes), encoder(a.Scopes), a.RedirectURI, a.Nonce, a.State,
			a.ForceApprovalPrompt, a.LoggedIn,
			a.Claims.UserID, a.Claims.Username, a.Claims.Email, a.Claims.EmailVerified,
			encoder(a.Claims.Groups),
			a.ConnectorID, a.ConnectorData,
			a.Expiry, r.ID,
		)
		if err != nil {
			return fmt.Errorf("update auth request: %v", err)
		}
		return nil
	})

}

func (c *conn) GetAuthRequest(id string) (storage.AuthRequest, error) {
	return getAuthRequest(c, id)
}

func getAuthRequest(q querier, id string) (a storage.AuthRequest, err error) {
	err = q.QueryRow(`
		select 
			id, client_id, response_types, scopes, redirect_uri, nonce, state,
			force_approval_prompt, logged_in,
			claims_user_id, claims_username, claims_email, claims_email_verified,
			claims_groups,
			connector_id, connector_data, expiry
		from auth_request where id = $1;
	`, id).Scan(
		&a.ID, &a.ClientID, decoder(&a.ResponseTypes), decoder(&a.Scopes), &a.RedirectURI, &a.Nonce, &a.State,
		&a.ForceApprovalPrompt, &a.LoggedIn,
		&a.Claims.UserID, &a.Claims.Username, &a.Claims.Email, &a.Claims.EmailVerified,
		decoder(&a.Claims.Groups),
		&a.ConnectorID, &a.ConnectorData, &a.Expiry,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return a, storage.ErrNotFound
		}
		return a, fmt.Errorf("select auth request: %v", err)
	}
	return a, nil
}

func (c *conn) CreateAuthCode(a storage.AuthCode) error {
	_, err := c.Exec(`
		insert into auth_code (
			id, client_id, scopes, nonce, redirect_uri,
			claims_user_id, claims_username,
			claims_email, claims_email_verified, claims_groups,
			connector_id, connector_data,
			expiry
		)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);
	`,
		a.ID, a.ClientID, encoder(a.Scopes), a.Nonce, a.RedirectURI, a.Claims.UserID,
		a.Claims.Username, a.Claims.Email, a.Claims.EmailVerified, encoder(a.Claims.Groups),
		a.ConnectorID, a.ConnectorData, a.Expiry,
	)
	return err
}

func (c *conn) GetAuthCode(id string) (a storage.AuthCode, err error) {
	err = c.QueryRow(`
		select
			id, client_id, scopes, nonce, redirect_uri,
			claims_user_id, claims_username,
			claims_email, claims_email_verified, claims_groups,
			connector_id, connector_data,
			expiry
		from auth_code where id = $1;
	`, id).Scan(
		&a.ID, &a.ClientID, decoder(&a.Scopes), &a.Nonce, &a.RedirectURI, &a.Claims.UserID,
		&a.Claims.Username, &a.Claims.Email, &a.Claims.EmailVerified, decoder(&a.Claims.Groups),
		&a.ConnectorID, &a.ConnectorData, &a.Expiry,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return a, storage.ErrNotFound
		}
		return a, fmt.Errorf("select auth code: %v", err)
	}
	return a, nil
}

func (c *conn) CreateRefresh(r storage.RefreshToken) error {
	_, err := c.Exec(`
		insert into refresh_token (
			id, client_id, scopes, nonce,
			claims_user_id, claims_username, claims_email, claims_email_verified,
			claims_groups,
			connector_id, connector_data
		)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);
	`,
		r.RefreshToken, r.ClientID, encoder(r.Scopes), r.Nonce,
		r.Claims.UserID, r.Claims.Username, r.Claims.Email, r.Claims.EmailVerified,
		encoder(r.Claims.Groups),
		r.ConnectorID, r.ConnectorData,
	)
	if err != nil {
		return fmt.Errorf("insert refresh_token: %v", err)
	}
	return nil
}

func (c *conn) GetRefresh(id string) (storage.RefreshToken, error) {
	return scanRefresh(c.QueryRow(`
		select
			id, client_id, scopes, nonce,
			claims_user_id, claims_username, claims_email, claims_email_verified,
			claims_groups,
			connector_id, connector_data
		from refresh_token where id = $1;
	`, id))
}

func (c *conn) ListRefreshTokens() ([]storage.RefreshToken, error) {
	rows, err := c.Query(`
		select
			id, client_id, scopes, nonce,
			claims_user_id, claims_username, claims_email, claims_email_verified,
			claims_groups,
			connector_id, connector_data
		from refresh_token;
	`)
	if err != nil {
		return nil, fmt.Errorf("query: %v", err)
	}
	var tokens []storage.RefreshToken
	for rows.Next() {
		r, err := scanRefresh(rows)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan: %v", err)
	}
	return tokens, nil
}

func scanRefresh(s scanner) (r storage.RefreshToken, err error) {
	err = s.Scan(
		&r.RefreshToken, &r.ClientID, decoder(&r.Scopes), &r.Nonce,
		&r.Claims.UserID, &r.Claims.Username, &r.Claims.Email, &r.Claims.EmailVerified,
		decoder(&r.Claims.Groups),
		&r.ConnectorID, &r.ConnectorData,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return r, storage.ErrNotFound
		}
		return r, fmt.Errorf("scan refresh_token: %v", err)
	}
	return r, nil
}

func (c *conn) UpdateKeys(updater func(old storage.Keys) (storage.Keys, error)) error {
	return c.ExecTx(func(tx *trans) error {
		firstUpdate := false
		// TODO(ericchiang): errors may cause a transaction be rolled back by the SQL
		// server. Test this, and consider adding a COUNT() command beforehand.
		old, err := getKeys(tx)
		if err != nil {
			if err != storage.ErrNotFound {
				return fmt.Errorf("get keys: %v", err)
			}
			firstUpdate = true
			old = storage.Keys{}
		}

		nk, err := updater(old)
		if err != nil {
			return err
		}

		if firstUpdate {
			_, err = tx.Exec(`
				insert into keys (
					id, verification_keys, signing_key, signing_key_pub, next_rotation
				)
				values ($1, $2, $3, $4, $5);
			`,
				keysRowID, encoder(nk.VerificationKeys), encoder(nk.SigningKey),
				encoder(nk.SigningKeyPub), nk.NextRotation,
			)
			if err != nil {
				return fmt.Errorf("insert: %v", err)
			}
		} else {
			_, err = tx.Exec(`
				update keys
				set 
				    verification_keys = $1,
					signing_key = $2,
					signing_key_pub = $3,
					next_rotation = $4
				where id = $5;
			`,
				encoder(nk.VerificationKeys), encoder(nk.SigningKey),
				encoder(nk.SigningKeyPub), nk.NextRotation, keysRowID,
			)
			if err != nil {
				return fmt.Errorf("update: %v", err)
			}
		}
		return nil
	})
}

func (c *conn) GetKeys() (keys storage.Keys, err error) {
	return getKeys(c)
}

func getKeys(q querier) (keys storage.Keys, err error) {
	err = q.QueryRow(`
		select
			verification_keys, signing_key, signing_key_pub, next_rotation
		from keys
		where id=$1
	`, keysRowID).Scan(
		decoder(&keys.VerificationKeys), decoder(&keys.SigningKey),
		decoder(&keys.SigningKeyPub), &keys.NextRotation,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return keys, storage.ErrNotFound
		}
		return keys, fmt.Errorf("query keys: %v", err)
	}
	return keys, nil
}

func (c *conn) UpdateClient(id string, updater func(old storage.Client) (storage.Client, error)) error {
	return c.ExecTx(func(tx *trans) error {
		cli, err := getClient(tx, id)
		if err != nil {
			return err
		}
		nc, err := updater(cli)
		if err != nil {
			return err
		}

		_, err = tx.Exec(`
			update client
			set
				secret = $1,
				redirect_uris = $2,
				trusted_peers = $3,
				public = $4,
				name = $5,
				logo_url = $6
			where id = $7;
		`, nc.Secret, encoder(nc.RedirectURIs), encoder(nc.TrustedPeers), nc.Public, nc.Name, nc.LogoURL, id,
		)
		if err != nil {
			return fmt.Errorf("update client: %v", err)
		}
		return nil
	})
}

func (c *conn) CreateClient(cli storage.Client) error {
	_, err := c.Exec(`
		insert into client (
			id, secret, redirect_uris, trusted_peers, public, name, logo_url
		)
		values ($1, $2, $3, $4, $5, $6, $7);
	`,
		cli.ID, cli.Secret, encoder(cli.RedirectURIs), encoder(cli.TrustedPeers),
		cli.Public, cli.Name, cli.LogoURL,
	)
	if err != nil {
		return fmt.Errorf("insert client: %v", err)
	}
	return nil
}

func getClient(q querier, id string) (storage.Client, error) {
	return scanClient(q.QueryRow(`
		select
			id, secret, redirect_uris, trusted_peers, public, name, logo_url
	    from client where id = $1;
	`, id))
}

func (c *conn) GetClient(id string) (storage.Client, error) {
	return getClient(c, id)
}

func (c *conn) ListClients() ([]storage.Client, error) {
	rows, err := c.Query(`
		select
			id, secret, redirect_uris, trusted_peers, public, name, logo_url
		from client;
	`)
	if err != nil {
		return nil, err
	}
	var clients []storage.Client
	for rows.Next() {
		cli, err := scanClient(rows)
		if err != nil {
			return nil, err
		}
		clients = append(clients, cli)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return clients, nil
}

func scanClient(s scanner) (cli storage.Client, err error) {
	err = s.Scan(
		&cli.ID, &cli.Secret, decoder(&cli.RedirectURIs), decoder(&cli.TrustedPeers),
		&cli.Public, &cli.Name, &cli.LogoURL,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return cli, storage.ErrNotFound
		}
		return cli, fmt.Errorf("get client: %v", err)
	}
	return cli, nil
}

func (c *conn) CreatePassword(p storage.Password) error {
	p.Email = strings.ToLower(p.Email)
	_, err := c.Exec(`
		insert into password (
			email, hash, username, user_id
		)
		values (
			$1, $2, $3, $4
		);
	`,
		p.Email, p.Hash, p.Username, p.UserID,
	)
	if err != nil {
		return fmt.Errorf("insert password: %v", err)
	}
	return nil
}

func (c *conn) UpdatePassword(email string, updater func(p storage.Password) (storage.Password, error)) error {
	return c.ExecTx(func(tx *trans) error {
		p, err := getPassword(tx, email)
		if err != nil {
			return err
		}

		np, err := updater(p)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`
			update password
			set
				hash = $1, username = $2, user_id = $3
			where email = $4;
		`,
			np.Hash, np.Username, np.UserID, p.Email,
		)
		if err != nil {
			return fmt.Errorf("update password: %v", err)
		}
		return nil
	})
}

func (c *conn) GetPassword(email string) (storage.Password, error) {
	return getPassword(c, email)
}

func getPassword(q querier, email string) (p storage.Password, err error) {
	email = strings.ToLower(email)
	err = q.QueryRow(`
		select
			email, hash, username, user_id
		from password where email = $1;
	`, email).Scan(
		&p.Email, &p.Hash, &p.Username, &p.UserID,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return p, storage.ErrNotFound
		}
		return p, fmt.Errorf("select password: %v", err)
	}
	return p, nil
}

func (c *conn) DeleteAuthRequest(id string) error { return c.delete("auth_request", "id", id) }
func (c *conn) DeleteAuthCode(id string) error    { return c.delete("auth_code", "id", id) }
func (c *conn) DeleteClient(id string) error      { return c.delete("client", "id", id) }
func (c *conn) DeleteRefresh(id string) error     { return c.delete("refresh_token", "id", id) }
func (c *conn) DeletePassword(email string) error {
	return c.delete("password", "email", strings.ToLower(email))
}

// Do NOT call directly. Does not escape table.
func (c *conn) delete(table, field, id string) error {
	result, err := c.Exec(`delete from `+table+` where `+field+` = $1`, id)
	if err != nil {
		return fmt.Errorf("delete %s: %v", table, id)
	}

	// For now mandate that the driver implements RowsAffected. If we ever need to support
	// a driver that doesn't implement this, we can run this in a transaction with a get beforehand.
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %v", err)
	}
	if n < 1 {
		return storage.ErrNotFound
	}
	return nil
}
