-- Docker observation is per-box and disabled unless explicitly selected.
ALTER TABLE seedboxes ADD COLUMN ssh_docker_metrics INTEGER NOT NULL DEFAULT 0;
