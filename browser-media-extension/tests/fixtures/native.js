// Existing behavior fixtures now emulate the host contract. Product validation
// stays strict; separate submission tests deliberately return legacy/invalid ACKs.
export const isTaskCreation = m => m.method === 'create' || m.params?.method === 'POST' && m.params?.path === '/api/v1/tasks';
export const taskPayload = m => m.method === 'create' ? m.params : m.params.data;
// Read-only identity frames are separate from each behavior fixture's business
// assertions. They never call its simulated create/REST side effects.
export function identityReply(message,extensionId='test') {
  if(message.method!=='hostInfo')return null;
  return {code:0,data:{version:1,product:'kownload',nativeProtocol:1,identityProtocol:1,
    build:{sourceCommit:'b'.repeat(40),releaseVersion:'fixture',preview:false},
    supportedHostNames:['io.github.kbwumin13_sudo.kownload'],callerOrigin:`chrome-extension://${extensionId}/`,routeScope:'a'.repeat(64),
    app:{reachable:true,identity:{version:1,product:'kownload',identityProtocol:1,routeScope:'a'.repeat(64),build:{preview:false}}}}};
}
export function contractReply(message, reply) {
  if (reply?.code !== 0 || message.method === 'wakeup' || message.method === 'ping') return reply;
  const data = {...reply.data,operationId:message.operationId};
  if (message.method === 'create' && data.code === 0 && !data.submission) data.submission = {state:'created',taskIds:[data.data]};
  return {...reply,operationId:message.operationId,httpStatus:reply.httpStatus ?? 200,businessCode:data.code,data,...(data.submission ? {submission:data.submission} : {})};
}
