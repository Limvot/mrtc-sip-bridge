-- v0 -> v1: Latest revision
-- Dialpad PWA device store (docs/PWA-SPEC.md §2.3). Times are unix milliseconds. Secrets are never
-- stored: code_hash and token_hash are hex sha256 of the full secret string.

-- One row per device = one token family.
CREATE TABLE mrtcsip_dialpad_device (
    bridge_id        TEXT    NOT NULL,
    id               TEXT    NOT NULL,
    state            TEXT    NOT NULL,
    code_hash        TEXT,
    code_expires     BIGINT,
    user_mxid        TEXT    NOT NULL,
    login_id         TEXT    NOT NULL,
    room_id          TEXT    NOT NULL DEFAULT '',
    name             TEXT    NOT NULL,
    client           TEXT    NOT NULL DEFAULT '',
    client_notice_at BIGINT,
    created_at       BIGINT  NOT NULL,
    enrolled_at      BIGINT,
    last_used_at     BIGINT,
    hard_expiry      BIGINT,
    revoked_at       BIGINT,
    revoked_why      TEXT,
    notice_room      TEXT,
    notice_event     TEXT,
    notice_tries     INTEGER NOT NULL DEFAULT 0,

    PRIMARY KEY (bridge_id, id)
);
CREATE UNIQUE INDEX mrtcsip_dialpad_device_code ON mrtcsip_dialpad_device (bridge_id, code_hash);
CREATE INDEX mrtcsip_dialpad_device_user ON mrtcsip_dialpad_device (bridge_id, user_mxid);

-- One row per issued device token (rotation chain).
CREATE TABLE mrtcsip_dialpad_token (
    bridge_id     TEXT    NOT NULL,
    token_hash    TEXT    NOT NULL,
    device_id     TEXT    NOT NULL,
    parent_hash   TEXT,
    seq           BIGINT  NOT NULL,
    issued_at     BIGINT  NOT NULL,
    confirmed_at  BIGINT,
    superseded_at BIGINT,

    PRIMARY KEY (bridge_id, token_hash),
    CONSTRAINT mrtcsip_dialpad_token_device_fkey FOREIGN KEY (bridge_id, device_id)
        REFERENCES mrtcsip_dialpad_device (bridge_id, id) ON DELETE CASCADE
);
CREATE INDEX mrtcsip_dialpad_token_device ON mrtcsip_dialpad_token (bridge_id, device_id);
CREATE INDEX mrtcsip_dialpad_token_parent ON mrtcsip_dialpad_token (bridge_id, parent_hash);
