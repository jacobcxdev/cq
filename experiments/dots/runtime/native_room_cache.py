"""Recover one cancelled shared Dot read inside the pinned native renderer.

The native cache can reuse a producer bound to a disposed chat controller.
Its cancellation must not fail a different, still-current consumer. Only GET
room/history cache reads are covered; the caller still checks its own lifetime
and account in its original fetch closure. No send is retried.
"""
import json

import isolated_native
import settings


GUARD = r"""(cache, rooms) => {
    const proto = Object.getPrototypeOf(cache);
    const key = Symbol.for('cq.dot-read-recovery.v1');
    if (proto[key]) return proto[key];
    const original = proto.read;
    const allowed = new Set(rooms);
    const account = cache.accountId, user = cache.userId;
    const state = {recovered: 0};
    const wrapped = function(map, room, fetch, signal) {
        const pending = Reflect.apply(original, this, arguments);
        if (!allowed.has(room) || this.accountId !== account || this.userId !== user)
            return pending;
        const entry = map.get(room);
        return pending.catch(error => {
            if (!entry || error?.name !== 'AbortError' || signal?.aborted || this.disposed
                    || !this.isCurrentAccount() || entry?.aborted) throw error;
            // A disposed caller's original closure rejects before doing I/O.
            // A current caller gets its own read instead of the cancelled producer.
            return fetch(this.abort.signal).then(value => {
                this.assertCurrent();
                state.recovered++;
                return value;
            });
        });
    };
    state.restore = () => {
        if (proto.read === wrapped) proto.read = original;
        delete proto[key];
        if (globalThis[key] === state) delete globalThis[key];
    };
    proto.read = wrapped;
    proto[key] = state;
    return state;
}"""


def install():
    if settings.INTEGRATION_MODE != 'native':
        return {'installed': False}
    expression = r"""(async()=>{
        const m = await import('app://-/assets/MODULE');
        const key = Symbol.for('cq.dot-read-recovery.v1');
        if (globalThis[key]) return {installed:true,recovered:globalThis[key].recovered};
        const root = document.getElementById('root');
        const property = root && Object.keys(root).find(k=>k.startsWith('__reactContainer$'));
        const container = property && root[property];
        const fibres = [container?.current ?? container];
        const seen = new Set();
        let scope;
        const inspect = (value, depth) => {
            if (scope || !value || typeof value !== 'object' || seen.has(value)) return;
            seen.add(value);
            try {
                if (typeof value.get === 'function' && typeof value.watch === 'function'
                        && typeof value.get(m.GMr) === 'string'
                        && typeof value.get(m.XMr) === 'string' && value.get(m.JMr)) {
                    scope = value;
                    return;
                }
                if (depth > 0) for (const field of Object.values(Object.getOwnPropertyDescriptors(value)))
                    if ('value' in field) inspect(field.value, depth - 1);
            } catch {}
        };
        for (let i=0; i<fibres.length && i<10000 && !scope; i++) {
            const fibre = fibres[i];
            if (!fibre) continue;
            inspect(fibre.memoizedProps, 3);
            let hook = fibre.memoizedState;
            for (let j=0; hook && j<100 && !scope; j++,hook=hook.next)
                inspect(hook.memoizedState, 3);
            if (fibre.child) fibres.push(fibre.child);
            if (fibre.sibling) fibres.push(fibre.sibling);
        }
        if (!scope) throw Error('Native messaging scope unavailable');
        const cache = m.Lpr(scope);
        if (!cache || typeof cache.read !== 'function' || cache.disposed)
            throw Error('Native messaging cache unavailable');
        const catalogue = await m.Ymr.safeGet('/tbo');
        const expected = new Set(PROFILES);
        const profiles = catalogue.items ?? [];
        if (profiles.length !== expected.size || profiles.some(p=>!expected.delete(p.id)
                || typeof p.messaging_room_id !== 'string' || !p.messaging_room_id) || expected.size)
            throw Error('Native catalogue identity changed');
        const state = (GUARD)(cache, profiles.map(p=>p.messaging_room_id));
        globalThis[key] = state;
        return {installed:true,recovered:state.recovered};
    })()"""
    expression = expression.replace('MODULE', settings.MODULE_ASSET)
    expression = expression.replace('PROFILES', json.dumps([p for p, _ in settings.DOTS.values()]))
    expression = expression.replace('GUARD', GUARD)
    results = isolated_native.evaluate_all(expression)
    ready = ([value for value in results if isinstance(value, dict) and value.get('installed') is True]
             if isinstance(results, list) else [])
    if not ready:
        raise ValueError('Native Dot read recovery unavailable')
    return {'installed': True, 'windows': len(ready), 'recovered': sum(value.get('recovered', 0) for value in ready)}
