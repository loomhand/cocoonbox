DELETE FROM connect_grants c WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = c.user_id);
DELETE FROM connect_flows c WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = c.user_id);
DELETE FROM connect_approvals c WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = c.user_id);
DELETE FROM connect_audit c WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = c.user_id);
DELETE FROM connect_door_keys c WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = c.user_id);

ALTER TABLE connect_grants ADD FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE;
ALTER TABLE connect_flows ADD FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE;
ALTER TABLE connect_approvals ADD FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE;
ALTER TABLE connect_audit ADD FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE;
ALTER TABLE connect_door_keys ADD FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE;
