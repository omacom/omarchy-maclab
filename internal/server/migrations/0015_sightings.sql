-- The Sightings labd holds, so that a restart of labd keeps them: which
-- Lab-owned Mac is on which port of the mini, the port's product id and
-- plug count, and the Mac's boot id, as they were last seen. A restarted
-- labd keeps one while its port is still connected with the same product
-- id; after that the usual rules go on from what is stored here.
CREATE TABLE sightings (
  mac TEXT PRIMARY KEY,
  rid INTEGER NOT NULL,
  product_id INTEGER NOT NULL,
  plug_count INTEGER NOT NULL,
  boot TEXT NOT NULL,
  proved INTEGER NOT NULL,
  at INTEGER NOT NULL
) STRICT;
