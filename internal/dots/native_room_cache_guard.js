(cache, rooms, api) => {
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
}