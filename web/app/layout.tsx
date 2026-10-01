import './style.css';
import Link from 'next/link';
export const metadata = { title: 'HomesByMe | Market observations', description: 'Local housing market history' };
export default function Layout({children}: {children: React.ReactNode}) {
 return <html lang="en"><body><header><Link href="/" className="brand">HomesByMe</Link><nav><Link href="/">Market dashboard</Link><Link href="/compare">Compare areas</Link></nav><span>LOCAL RESEARCH</span></header><main>{children}</main><footer>Observed listing activity · Inactive does not mean pending or sold.</footer></body></html>;
}
