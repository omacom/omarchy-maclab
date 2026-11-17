-- Who approved a Release's manifest, by account id, as a manifest frozen
-- from 2026-11-16 names them. The page shows each by the handle the lab
-- knows them by; a forgetting takes the person's rows away, while the
-- frozen manifest keeps its bytes.
CREATE TABLE release_approvals (
  release TEXT NOT NULL,
  position INTEGER NOT NULL,
  account_id INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (release, position)
) STRICT;
