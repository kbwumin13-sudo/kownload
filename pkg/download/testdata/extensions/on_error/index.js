gopeed.events.onError(async function (ctx) {
    gopeed.logger.info("url", ctx.task.meta.req.url);
    gopeed.logger.info("error", ctx.error);
    ctx.task.meta.req.url = ctx.task.meta.req.labels['fixtureUrl'];
    ctx.task.continue();
});

