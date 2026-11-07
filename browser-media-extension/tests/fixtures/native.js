// Existing behavior fixtures now emulate the host contract. Product validation
// stays strict; separate submission tests deliberately return legacy/invalid ACKs.
export const isTaskCreation = m => m.method === 'create' || m.params?.method === 'POST' && m.params?.path === '/api/v1/tasks';
export const taskPayload = m => m.method === 'create' ? m.params : m.params.data;
export function contractReply(message, reply) {
  if (reply?.code !== 0 || message.method === 'wakeup' || message.method === 'ping') return reply;
  const data = {...reply.data,operationId:message.operationId};
  if (message.method === 'create' && data.code === 0 && !data.submission) data.submission = {state:'created',taskIds:[data.data]};
  return {...reply,operationId:message.operationId,httpStatus:reply.httpStatus ?? 200,businessCode:data.code,data,...(data.submission ? {submission:data.submission} : {})};
}
