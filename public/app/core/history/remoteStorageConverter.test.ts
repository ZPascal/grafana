import { RichHistoryQuery } from '../../types';
import { backendSrv } from '../services/backend_srv';

import { RichHistoryRemoteStorageDTO } from './RichHistoryRemoteStorage';
import { DataSourceSrvMock } from './RichHistoryStorage';
import { fromDTO } from './remoteStorageConverter';

jest.mock('@grafana/runtime', () => ({
  ...jest.requireActual('@grafana/runtime'),
  getBackendSrv: () => backendSrv,
  getDataSourceSrv: () => DataSourceSrvMock,
}));

const validRichHistory: RichHistoryQuery = {
  comment: 'comment',
  createdAt: 1,
  datasourceName: 'name-of-dev-test',
  datasourceUid: 'dev-test',
  id: 'ID',
  queries: [{ refId: 'A' }],
  starred: true,
};

const validDTO: RichHistoryRemoteStorageDTO = {
  comment: 'comment',
  datasourceUid: 'dev-test',
  queries: [{ refId: 'A' }],
  starred: true,
  uid: 'ID',
  createdAt: 1,
};

describe('RemoteStorage converter', () => {
  it('converts DTO to RichHistoryQuery', () => {
    expect(fromDTO(validDTO)).toMatchObject(validRichHistory);
  });
});
