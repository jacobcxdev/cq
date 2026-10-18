"""Give shared Dot reads the cache's lifetime in the pinned native renderer.

The native cache can reuse a producer bound to a disposed chat controller.
Its cancellation must not fail a different, still-current consumer. Only the
cache's room and first history page GETs are covered. The native client still
checks its own lifetime before consuming the result. No send is retried.
"""
import json

import isolated_native
import settings


GUARD = r"""(cache, rooms, api) => {
    const proto = Object.getPrototypeOf(cache);
    const key = Symbol.for('cq.dot-read-ownership.v2');
    if (proto[key]) return proto[key];
    const original = proto.read;
    const allowed = new Set(rooms);
    const account = cache.accountId, user = cache.userId;
    const state = {ownedReads: 0};
    const wrapped = function(map, room, fetch, signal) {
        if (!allowed.has(room) || this.accountId !== account || this.userId !== user
                || (map !== this.rooms && map !== this.history))
            return Reflect.apply(original, this, arguments);
        const history = map === this.history;
        // A shared producer must not close over the first chat controller.
        // The native cache supplies its own signal and cancels unused entries.
        const producer = async signal => {
            this.assertCurrent();
            state.ownedReads++;
            const value = await api.safeGet(history
                ? '/messaging/rooms/{room_id}/messages' : '/messaging/rooms/{room_id}', {
                additionalHeaders: {'ChatGPT-Account-Id': account},
                expectedErrorStatuses: [401, 403, 404], retry: false, signal,
                parameters: {path: {room_id: room}, ...(history ? {query: {limit: 20}} : {})}
            });
            this.assertCurrent();
            return value;
        };
        return Reflect.apply(original, this, [map, room, producer, signal]);
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
        const key = Symbol.for('cq.dot-read-ownership.v2');
        if (globalThis[key]) return {installed:true,ownedReads:globalThis[key].ownedReads};
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
        const state = (GUARD)(cache, profiles.map(p=>p.messaging_room_id), m.Ymr);
        globalThis[key] = state;
        return {installed:true,ownedReads:state.ownedReads};
    })()"""
    expression = expression.replace('MODULE', settings.MODULE_ASSET)
    expression = expression.replace('PROFILES', json.dumps([p for p, _ in settings.DOTS.values()]))
    expression = expression.replace('GUARD', GUARD)
    results = isolated_native.evaluate_all(expression)
    ready = ([value for value in results if isinstance(value, dict) and value.get('installed') is True]
             if isinstance(results, list) else [])
    if not ready:
        raise ValueError('Native Dot read recovery unavailable')
    return {'installed': True, 'windows': len(ready), 'ownedReads': sum(value.get('ownedReads', 0) for value in ready)}
