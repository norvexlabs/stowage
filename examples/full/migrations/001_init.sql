-- 001: the initial schema
CREATE TABLE organisations (name text PRIMARY KEY);
CREATE TABLE users (email text PRIMARY KEY, password_sha256 text NOT NULL, admin boolean NOT NULL DEFAULT false);
CREATE TABLE api_keys (id bigserial PRIMARY KEY, owner text NOT NULL REFERENCES users (email), key_sha256 text NOT NULL);
CREATE TABLE settings (key text PRIMARY KEY, value text NOT NULL);
CREATE TABLE events (id bigserial PRIMARY KEY, received timestamptz NOT NULL DEFAULT now(), body jsonb NOT NULL);
