package publisher

// SetAfterCreate installs the crash hook used by tests.
func (p *Publisher) SetAfterCreate(f func() error) { p.afterCreate = f }
