-- Packages the lab adds (spec section 22, "Packages the lab adds"): an
-- Omarchy value measured on its Release with packages the lab added for it
-- names them, "omarchy-mlx" or "a+b", apart from the Release's own values.
-- A value with none is the Release's own; a macOS value never has any. A
-- metric is measured with packages added or as the Release's own, never
-- both, so perf_values_one still holds one value per metric and Release.
ALTER TABLE perf_values ADD COLUMN adds TEXT NOT NULL DEFAULT '' CHECK (adds = '' OR (side = 'omarchy' AND adds NOT GLOB '*[^a-z0-9@._+-]*'));
