gopeed.events.onStart(async function (ctx) {
    gopeed.logger.info("url", ctx.task.meta.req.url);
    ctx.task.meta.req.url = ctx.task.meta.req.labels['fixtureUrl'];
    ctx.task.meta.req.labels['modified'] = 'true';
});

