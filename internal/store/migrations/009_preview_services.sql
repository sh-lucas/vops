-- the services a preview is for (json list, from pushes and --image; [] = every service with x-vops.preview):
-- with their x-vops.preview.with and copy, they decide what the preview runs.
ALTER TABLE previews ADD COLUMN services TEXT NOT NULL DEFAULT '[]';
