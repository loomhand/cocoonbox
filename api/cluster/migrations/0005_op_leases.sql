UPDATE machines SET op = '' WHERE op <> '' AND op_until IS NULL;
