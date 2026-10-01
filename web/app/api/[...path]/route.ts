import { NextRequest } from 'next/server';
export async function GET(request: NextRequest) {
  const target = new URL(request.nextUrl.pathname + request.nextUrl.search, process.env.API_URL || 'http://localhost:18080');
  try {
    const response = await fetch(target, { cache: 'no-store', signal: AbortSignal.timeout(15000) });
    return new Response(await response.text(), { status: response.status, headers: { 'Content-Type': response.headers.get('Content-Type') || 'application/json' } });
  } catch { return Response.json({ error: 'Backend unavailable. Start the Go API and database.' }, { status: 502 }); }
}
