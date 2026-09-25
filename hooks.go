package orm

import "context"

type BeforeCreateHook interface{ BeforeCreate(context.Context) error }
type AfterCreateHook interface{ AfterCreate(context.Context) error }
type BeforeUpdateHook interface{ BeforeUpdate(context.Context) error }
type AfterUpdateHook interface{ AfterUpdate(context.Context) error }
type BeforeDeleteHook interface{ BeforeDelete(context.Context) error }
type AfterDeleteHook interface{ AfterDelete(context.Context) error }

func runBeforeCreate(ctx context.Context, entity any) error {
	if hook, ok := entity.(BeforeCreateHook); ok {
		return hook.BeforeCreate(ctx)
	}
	return nil
}

func runAfterCreate(ctx context.Context, entity any) error {
	if hook, ok := entity.(AfterCreateHook); ok {
		return hook.AfterCreate(ctx)
	}
	return nil
}

func runBeforeUpdate(ctx context.Context, entity any) error {
	if hook, ok := entity.(BeforeUpdateHook); ok {
		return hook.BeforeUpdate(ctx)
	}
	return nil
}

func runAfterUpdate(ctx context.Context, entity any) error {
	if hook, ok := entity.(AfterUpdateHook); ok {
		return hook.AfterUpdate(ctx)
	}
	return nil
}

func runBeforeDelete(ctx context.Context, entity any) error {
	if hook, ok := entity.(BeforeDeleteHook); ok {
		return hook.BeforeDelete(ctx)
	}
	return nil
}

func runAfterDelete(ctx context.Context, entity any) error {
	if hook, ok := entity.(AfterDeleteHook); ok {
		return hook.AfterDelete(ctx)
	}
	return nil
}
